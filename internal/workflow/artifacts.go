package workflow

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	uploadArtifactAction   = "actions/upload-artifact"
	downloadArtifactAction = "actions/download-artifact"
)

// hiddenFiles is what upload-artifact leaves out unless told otherwise. Since
// its v4.4 a hidden file is excluded by default — GitHub changed it so a
// workflow uploading `dist` or `.` stops shipping `.env` and `.git` with it —
// and §17's globs match a leading dot like any other character, so without
// these two patterns the shim would upload exactly what that change keeps out.
var hiddenFiles = []string{"**/.*", "**/.*/**"}

// artifactShim turns upload-artifact and download-artifact into a step the run
// loop performs itself (A49). ok is false for any other action.
//
// Like the checkout and cache shims it is asked before the allowlist: nothing
// is fetched and nothing third-party runs. Unlike them the step survives
// compilation, because whether it happens is decided by its `if:` at run time.
func artifactShim(st Step, sc expr.Scope) (spec *registry.ArtifactStep, outputs map[string]*expr.Program, ok bool, err error) {
	ref, parseErr := actions.ParseRef(st.Uses)
	if parseErr != nil {
		return nil, nil, false, nil
	}
	slug := strings.ToLower(ref.Slug())
	if slug != uploadArtifactAction && slug != downloadArtifactAction {
		return nil, nil, false, nil
	}
	if ref.Path != "" {
		return nil, nil, true, fmt.Errorf("uses: %s is not implemented; the upload and download actions are", st.Uses)
	}
	with, err := withStrings(st.With)
	if err != nil {
		return nil, nil, true, err
	}
	if slug == uploadArtifactAction {
		return uploadShim(st, with, sc)
	}
	return downloadShim(st, with, sc)
}

func uploadShim(st Step, with map[string]string, sc expr.Scope) (*registry.ArtifactStep, map[string]*expr.Program, bool, error) {
	spec := &registry.ArtifactStep{Name: "artifact", IfNoFilesFound: "warn"}
	var lateName registry.LateValue
	hidden := false
	for _, k := range sortedNames(with) {
		v := strings.TrimSpace(with[k])
		switch strings.ToLower(k) {
		case "name":
			name, late, err := artifactValue(st.Uses, "name", v, sc)
			if err != nil {
				return nil, nil, true, err
			}
			spec.Name, lateName = name, late
		case "path":
		case "if-no-files-found":
			spec.IfNoFilesFound = strings.ToLower(v)
		case "retention-days":
			if v == "" || v == "0" {
				continue
			}
			days, err := strconv.Atoi(v)
			if err != nil || days < 0 {
				return nil, nil, true, fmt.Errorf("uses: %s retention-days %q is not a number of days", st.Uses, v)
			}
			spec.Retain = protocol.Duration(time.Duration(days) * 24 * time.Hour)
		case "include-hidden-files":
			hidden = truthy(v) && v != ""
		case "archive":
			// v7: false uploads one file as it is, named after the file. It
			// decides what the step is, so it has to be known at admission.
			a, err := staticValue(st.Uses, "archive", v, sc)
			if err != nil {
				return nil, nil, true, err
			}
			if !truthy(a) && !strings.EqualFold(a, "false") {
				return nil, nil, true, fmt.Errorf("uses: %s archive %q is not true or false", st.Uses, a)
			}
			spec.Single = strings.EqualFold(a, "false")
		case "compression-level", "overwrite":
			// Nothing is zipped, and a second upload under one name adds to
			// the same directory rather than failing — more permissive than
			// GitHub, and harmless.
		default:
			return nil, nil, true, fmt.Errorf("uses: %s has an input latem does not know: %s", st.Uses, k)
		}
	}
	if spec.Single {
		// GitHub ignores the name, and so does this.
		spec.Name, lateName = "", nil
	}

	raw := with["path"]
	if strings.TrimSpace(raw) == "" {
		return nil, nil, true, fmt.Errorf("uses: %s requires the input \"path\"", st.Uses)
	}
	type line struct {
		exclude bool
		static  string
		late    registry.LateValue
	}
	var lines []line
	anyLate := false
	rooted := workspaceRooted(sc)
	for _, text := range strings.Split(raw, "\n") {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		exclude := strings.HasPrefix(text, "!")
		p, late, err := artifactValue(st.Uses, "path", strings.TrimPrefix(text, "!"), rooted)
		if err != nil {
			return nil, nil, true, err
		}
		if late != nil {
			// Placed in the workspace or runner.temp once it is known (A77).
			anyLate = true
			lines = append(lines, line{exclude: exclude, late: late})
			continue
		}
		p, err = workspaceRelative(st.Uses, p)
		if err != nil {
			return nil, nil, true, err
		}
		// The whole checkout. The collector refuses "." on its own, as a
		// request nobody means by accident; here it was written, and means
		// everything — less the hidden files, which is the point of them.
		if p == "." || p == "./" {
			p = "**"
		}
		lines = append(lines, line{exclude: exclude, static: p})
	}
	if anyLate || lateName != nil {
		spec.Late = &registry.ArtifactLate{Name: lateName}
		if lateName != nil {
			spec.Name = ""
		}
	}
	for _, l := range lines {
		switch {
		case anyLate:
			// One late line makes them all late, so that the run loop places
			// every line in the same directory and refuses two.
			v := l.late
			if v == nil {
				v = newLate(expr.Literal(l.static), rooted)
			}
			if l.exclude {
				spec.Late.Exclude = append(spec.Late.Exclude, v)
			} else {
				spec.Late.Include = append(spec.Late.Include, v)
			}
		case l.exclude:
			spec.Exclude = append(spec.Exclude, l.static)
		default:
			spec.Include = append(spec.Include, l.static)
		}
	}
	if !hidden {
		spec.Exclude = append(spec.Exclude, hiddenFiles...)
	}

	// What is known at admission is folded, through A44's output machinery.
	// A name built from a step's outputs, or one taken from the file
	// (archive: false), is only known as the step runs, and so are the
	// outputs: the run loop sets them then (A77).
	if spec.Late != nil || spec.Single {
		return spec, nil, true, nil
	}
	// artifact-id is the name, which is how this store knows it; there is no
	// web page to link to, so the url is empty rather than invented, and
	// there is no zip to take a digest of.
	outputs := map[string]*expr.Program{
		"artifact-id":     expr.Literal(spec.Name),
		"artifact-url":    expr.Literal(""),
		"artifact-digest": expr.Literal(""),
	}
	return spec, outputs, true, nil
}

func downloadShim(st Step, with map[string]string, sc expr.Scope) (*registry.ArtifactStep, map[string]*expr.Program, bool, error) {
	spec := &registry.ArtifactStep{Download: true}
	var lateName, latePath registry.LateValue
	for _, k := range sortedNames(with) {
		v := strings.TrimSpace(with[k])
		switch strings.ToLower(k) {
		case "name":
			name, late, err := artifactValue(st.Uses, "name", v, sc)
			if err != nil {
				return nil, nil, true, err
			}
			spec.Name, lateName = name, late
		case "path":
			p, late, err := artifactValue(st.Uses, "path", v, workspaceRooted(sc))
			if err != nil {
				return nil, nil, true, err
			}
			if late != nil {
				// runner.temp, or a step's outputs: placed as the step runs
				// (A77).
				latePath = late
				continue
			}
			if p, err = workspaceRelative(st.Uses, p); err != nil {
				return nil, nil, true, err
			}
			spec.Path = p
		case "run-id", "github-token", "repository":
			return nil, nil, true, fmt.Errorf(
				"uses: %s — %s asks for an artifact from another run. A run's artifacts are on the "+
					"machine that ran it, and this one has only its own", st.Uses, k)
		case "pattern", "merge-multiple":
			return nil, nil, true, fmt.Errorf(
				"uses: %s — %s selects across artifacts from several jobs, which is a matrix or a "+
					"job graph; latem runs one job per run (N2), so name the artifact instead", st.Uses, k)
		default:
			return nil, nil, true, fmt.Errorf("uses: %s has an input latem does not know: %s", st.Uses, k)
		}
	}
	if lateName != nil || latePath != nil {
		spec.Late = &registry.ArtifactLate{Name: lateName, Path: latePath}
	}
	if latePath != nil {
		return spec, nil, true, nil
	}
	// Relative to the workspace, which is where a step starts, so it resolves
	// the same on every executor without knowing where the workspace is.
	dest := spec.Path
	if dest == "" {
		dest = "."
	}
	return spec, map[string]*expr.Program{"download-path": expr.Literal(dest)}, true, nil
}

// artifactValue compiles one `with:` value of an artifact step: the text, when
// it is known at admission, or a late value, when it reads a step's outputs,
// the runner or the job (A77).
//
// A secret is refused by name. A name and a path are written where a run's
// artifacts are listed and sent to the control plane with them (§17), and
// neither is a place for a credential to end up.
func artifactValue(uses, field, v string, sc expr.Scope) (string, registry.LateValue, error) {
	if !expr.Has(v) {
		return v, nil, nil
	}
	spans, err := expr.Spans(v)
	if err != nil {
		return "", nil, fmt.Errorf("uses: %s %s: %w", uses, field, err)
	}
	anyLate := false
	for _, sp := range spans {
		contexts, _ := sp.Prog.References()
		keys, _ := sp.Prog.ContextKeys("github")
		if slices.Contains(contexts, secretsContext) || slices.Contains(keys, "token") {
			return "", nil, fmt.Errorf(
				"uses: %s %s %q reads a secret, and an artifact's name and paths are listed with the run's "+
					"artifacts, where no secret belongs", uses, field, v)
		}
		w, err := classify(sp.Prog, sc)
		if err != nil {
			return "", nil, fmt.Errorf("uses: %s %s: %w", uses, field, err)
		}
		if w == whenLate {
			anyLate = true
		}
	}
	if anyLate {
		if len(spans) == 1 && spans[0].Start == 0 && spans[0].End == len(v) {
			return "", newLate(spans[0].Prog, sc), nil
		}
		return "", newLateTemplate(v, spans, sc), nil
	}
	out, err := expr.EvalString(v, sc)
	if err != nil {
		return "", nil, fmt.Errorf("uses: %s %s: %w", uses, field, err)
	}
	return strings.TrimSpace(out), nil, nil
}

// staticValue evaluates a `with:` value that has to be known before the step
// runs, because it decides what the step is. A step's outputs, the job's
// status and the runner are refused by name.
func staticValue(uses, field, v string, sc expr.Scope) (string, error) {
	s, late, err := artifactValue(uses, field, v, sc)
	if err != nil {
		return "", err
	}
	if late != nil {
		return "", fmt.Errorf(
			"uses: %s %s %q is only known once the job is running, and it decides what this step is, "+
				"which is fixed when the run is admitted", uses, field, v)
	}
	return s, nil
}

// downloadable refuses a download that nothing could satisfy, and says which
// other jobs' artifacts it may read.
//
// GitHub's artifacts belong to the workflow run, so one job downloads what
// another uploaded. latem's belong to a run, which is one job, and stay on the
// machine that ran it (§17). A download finds what an earlier step of its own
// job uploaded, and what a job it needs uploaded, which the control plane
// places it beside (A84): spec.From names those jobs, from the plan of the
// pinned commit. With neither, the job would be admitted and fail at the step
// — and `workflows check` would say it can run.
//
// A name only known as either step runs could be anything, so it is let
// through to be answered then.
func (x *expander) downloadable(uses string, spec *registry.ArtifactStep) error {
	if !x.fromAsked && x.artifactsFrom != nil {
		x.from, x.fromAsked = x.artifactsFrom(), true
	}
	spec.From = slices.Clone(x.from)
	if len(spec.From) > 0 {
		return nil
	}
	const why = "a download finds what an earlier step of its job uploaded, or what a job it needs " +
		"uploaded, on the machine that ran that job (A77, A84)"
	if len(x.uploads) == 0 {
		return fmt.Errorf("uses: %s — no step before this one in the job, and no job it needs, uploads "+
			"an artifact; %s", uses, why)
	}
	if spec.Name == "" || (spec.Late != nil && spec.Late.Name != nil) {
		return nil
	}
	for _, n := range x.uploads {
		if n == "" || n == spec.Name {
			return nil
		}
	}
	return fmt.Errorf("uses: %s — no step before this one in the job, and no job it needs, uploads an "+
		"artifact named %q; %s", uses, spec.Name, why)
}

// uploadName is the name an upload is known by at admission, or "" when it
// is only known as the step runs.
func uploadName(spec *registry.ArtifactStep) string {
	if spec.Single || (spec.Late != nil && spec.Late.Name != nil) {
		return ""
	}
	return spec.Name
}

// workspaceRooted is sc with github.workspace set to ".", the collector's own
// frame. `${{ github.workspace }}/dist` is the commonest way a workflow spells a
// path in its checkout, and evaluated this way it becomes `./dist` whatever
// the workspace's absolute path is — which differs between the host and a
// container, and so is the one form of the answer that cannot be wrong on
// either executor.
func workspaceRooted(sc expr.Scope) expr.Scope {
	out := sc
	out.Contexts = make(map[string]expr.Value, len(sc.Contexts))
	for k, v := range sc.Contexts {
		out.Contexts[k] = v
	}
	gh := map[string]any{}
	if orig, ok := sc.Contexts["github"].(map[string]any); ok {
		for k, v := range orig {
			gh[k] = v
		}
	}
	gh["workspace"] = "."
	out.Contexts["github"] = gh
	return out
}

// workspaceRelative normalises a path known at admission, which is in the
// workspace, and refuses one anywhere else. runner.temp is the other place an
// artifact step reaches, and is only known as the step runs, so a path in it
// is late and placed then (A77); a file in $HOME has to be copied in first.
func workspaceRelative(uses, p string) (string, error) {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return "", fmt.Errorf(
			"uses: %s path %q is outside the workspace and runner.temp, the two places an artifact "+
				"step reaches (§17, A77); copy the file into one of them in an earlier step", uses, p)
	}
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	if p == "" {
		p = "."
	}
	return p, nil
}
