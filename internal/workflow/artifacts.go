package workflow

import (
	"fmt"
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
	hidden := false
	for _, k := range sortedNames(with) {
		v := strings.TrimSpace(with[k])
		switch strings.ToLower(k) {
		case "name":
			name, err := staticValue(st.Uses, "name", v, sc)
			if err != nil {
				return nil, nil, true, err
			}
			spec.Name = name
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
		case "compression-level", "overwrite":
			// Nothing is zipped, and a second upload under one name adds to
			// the same directory rather than failing — more permissive than
			// GitHub, and harmless.
		default:
			return nil, nil, true, fmt.Errorf("uses: %s has an input latem does not know: %s", st.Uses, k)
		}
	}

	raw := with["path"]
	if strings.TrimSpace(raw) == "" {
		return nil, nil, true, fmt.Errorf("uses: %s requires the input \"path\"", st.Uses)
	}
	rooted := workspaceRooted(sc)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		exclude := strings.HasPrefix(line, "!")
		p, err := staticValue(st.Uses, "path", strings.TrimPrefix(line, "!"), rooted)
		if err != nil {
			return nil, nil, true, err
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
		if exclude {
			spec.Exclude = append(spec.Exclude, p)
		} else {
			spec.Include = append(spec.Include, p)
		}
	}
	if !hidden {
		spec.Exclude = append(spec.Exclude, hiddenFiles...)
	}

	// artifact-id is the name, which is how this store knows it; there is no
	// web page to link to, so the url is empty rather than invented.
	outputs := map[string]*expr.Program{
		"artifact-id":  expr.Literal(spec.Name),
		"artifact-url": expr.Literal(""),
	}
	return spec, outputs, true, nil
}

func downloadShim(st Step, with map[string]string, sc expr.Scope) (*registry.ArtifactStep, map[string]*expr.Program, bool, error) {
	spec := &registry.ArtifactStep{Download: true}
	for _, k := range sortedNames(with) {
		v := strings.TrimSpace(with[k])
		switch strings.ToLower(k) {
		case "name":
			name, err := staticValue(st.Uses, "name", v, sc)
			if err != nil {
				return nil, nil, true, err
			}
			spec.Name = name
		case "path":
			p, err := staticValue(st.Uses, "path", v, workspaceRooted(sc))
			if err != nil {
				return nil, nil, true, err
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
	// Relative to the workspace, which is where a step starts, so it resolves
	// the same on every executor without knowing where the workspace is.
	dest := spec.Path
	if dest == "" {
		dest = "."
	}
	return spec, map[string]*expr.Program{"download-path": expr.Literal(dest)}, true, nil
}

// staticValue evaluates a `with:` value that has to be known before the step
// runs — a name or a path. A step's outputs, the job's status and the runner
// are refused by name, because the registry step is fixed at admission.
func staticValue(uses, field, v string, sc expr.Scope) (string, error) {
	if !expr.Has(v) {
		return v, nil
	}
	spans, err := expr.Spans(v)
	if err != nil {
		return "", fmt.Errorf("uses: %s %s: %w", uses, field, err)
	}
	for _, sp := range spans {
		w, err := classify(sp.Prog, sc)
		if err != nil {
			return "", fmt.Errorf("uses: %s %s: %w", uses, field, err)
		}
		if w == whenLate {
			return "", fmt.Errorf(
				"uses: %s %s %q is only known once the job is running, and this step is fixed when "+
					"the run is admitted", uses, field, v)
		}
	}
	out, err := expr.EvalString(v, sc)
	if err != nil {
		return "", fmt.Errorf("uses: %s %s: %w", uses, field, err)
	}
	return strings.TrimSpace(out), nil
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

// workspaceRelative normalises a path in the workspace and refuses one
// anywhere else. Artifacts are collected from the checkout (§17); a file in
// runner.temp or $HOME has to be copied in first.
func workspaceRelative(uses, p string) (string, error) {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return "", fmt.Errorf(
			"uses: %s path %q is outside the workspace. Artifacts are collected from the checkout "+
				"(§17); copy the file into it in an earlier step", uses, p)
	}
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	if p == "" {
		p = "."
	}
	return p, nil
}
