package run

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runArtifactStep performs an upload-artifact or download-artifact step
// (A49).
//
// It is the run loop's rather than an executor's because both ends of the
// copy — the workspace and this run's artifact store — are host paths the
// agent owns, on docker as on native; handing it to an executor would mean a
// container reaching the artifact store, which is exactly the boundary §17
// keeps.
//
// What was only known as the step runs — a name built from a step's outputs,
// a path in runner.temp or read from a step's outputs — is resolved here,
// and the step's outputs are what actually happened (A77).
func (x *execution) runArtifactStep(
	ctx context.Context, s registry.ResolvedStep, label string, sc registry.StepScope,
) (outcome, map[string]string) {
	a := s.Artifact
	if x.o.artifacts == nil || x.ws == nil {
		return fail(errors.New("this agent has no artifact store"), protocol.ErrArtifact), nil
	}
	name, err := resolveArtifactValue(a.Name, lateName(a), "name", sc)
	if err != nil {
		return fail(err, protocol.ErrBadRegistry), nil
	}
	if a.Download {
		return x.download(a, name, label, sc)
	}
	return x.upload(ctx, a, name, label, sc)
}

func (x *execution) download(a *registry.ArtifactStep, name, label string, sc registry.StepScope) (outcome, map[string]string) {
	where, dest := x.artifactPlaces()[0], a.Path
	if a.Late != nil && a.Late.Path != nil {
		raw, err := a.Late.Path.Resolve(sc)
		if err != nil {
			return fail(&registry.Error{Code: protocol.ErrBadRegistry, Msg: "artifact download path", Err: err},
				protocol.ErrBadRegistry), nil
		}
		if where, dest, err = x.placeArtifactPath(strings.TrimSpace(raw)); err != nil {
			return artifactRefused(label, err), nil
		}
	}
	if dest == "" {
		dest = "."
	}
	n, err := x.o.artifacts.Restore(x.run.ID, where.host, name, dest)
	if err != nil {
		if errors.Is(err, artifact.ErrNoSuchArtifact) {
			return outcome{
				state: protocol.StateFailed,
				code:  protocol.ErrArtifact,
				msg: fmt.Sprintf("step %q: %v — download-artifact reaches artifacts this run "+
					"uploaded, and one from another job or run is not something a single run has", label, err),
			}, nil
		}
		return fail(err, protocol.ErrArtifact), nil
	}
	x.note("step %s: restored %d file(s)", label, n)
	return outcome{state: protocol.StateSucceeded}, map[string]string{"download-path": where.join(dest)}
}

func (x *execution) upload(
	ctx context.Context, a *registry.ArtifactStep, name, label string, sc registry.StepScope,
) (outcome, map[string]string) {
	where := x.artifactPlaces()[0]
	include, exclude := a.Include, a.Exclude
	if a.Late != nil && len(a.Late.Include) > 0 {
		var err error
		if where, include, exclude, err = x.placeLatePaths(a, sc); err != nil {
			var reg *registry.Error
			if errors.As(err, &reg) {
				return fail(err, protocol.ErrBadRegistry), nil
			}
			return artifactRefused(label, err), nil
		}
	}

	refs, err := x.o.artifacts.CollectNamed(ctx, x.run.ID, where.host, artifact.Upload{
		Name:    name,
		Include: include,
		Exclude: exclude,
		Retain:  a.Retain.Duration(),
		Single:  a.Single,
	})
	if err != nil {
		return fail(err, protocol.ErrArtifact), nil
	}
	if len(refs) == 0 {
		paths := include
		if len(paths) == 0 && a.Late != nil {
			for _, v := range a.Late.Include {
				paths = append(paths, v.Source())
			}
		}
		what := fmt.Sprintf("artifact %q", name)
		if a.Single {
			what = "the artifact (archive: false)"
		}
		switch a.IfNoFilesFound {
		case "error":
			return outcome{
				state: protocol.StateFailed,
				code:  protocol.ErrArtifact,
				msg:   fmt.Sprintf("step %q: nothing matched %v in %s for %s", label, paths, where.name, what),
			}, nil
		case "warn":
			x.note("step %s: nothing matched %v in %s; %s has no files", label, paths, where.name, what)
		}
		return outcome{state: protocol.StateSucceeded}, map[string]string{"artifact-id": "", "artifact-url": "", "artifact-digest": ""}
	}
	digest := ""
	if a.Single {
		// The file is the artifact, so its digest is the artifact's, as
		// GitHub's is for an unarchived upload.
		name, _, _ = strings.Cut(refs[0].Path, "/")
		digest = refs[0].SHA256
	}
	x.stepArtifacts = append(x.stepArtifacts, refs...)
	x.note("step %s: uploaded artifact %q from %s, %d file(s)", label, name, where.name, len(refs))
	// artifact-id is the name, which is how this store knows it; there is no
	// web page to link to, so the url is empty rather than invented.
	return outcome{state: protocol.StateSucceeded}, map[string]string{
		"artifact-id": name, "artifact-url": "", "artifact-digest": digest,
	}
}

func lateName(a *registry.ArtifactStep) registry.LateValue {
	if a.Late == nil {
		return nil
	}
	return a.Late.Name
}

// resolveArtifactValue is the static value, or the late one resolved now
// against the scope the step runs in.
func resolveArtifactValue(static string, late registry.LateValue, field string, sc registry.StepScope) (string, error) {
	if late == nil {
		return static, nil
	}
	v, err := late.Resolve(sc)
	if err != nil {
		return "", &registry.Error{Code: protocol.ErrBadRegistry, Msg: "artifact " + field, Err: err}
	}
	return strings.TrimSpace(v), nil
}

// placeLatePaths resolves an upload's late `path:` lines and places them. The
// lines that resolve to nothing — an output a skipped step never wrote — are
// left out, and an upload left with no lines matches nothing, which its
// if-no-files-found decides about.
//
// Every line has to be in the same place. upload-artifact roots an artifact at
// the directory its paths share, and the workspace and runner.temp share none
// a step could name.
func (x *execution) placeLatePaths(a *registry.ArtifactStep, sc registry.StepScope) (
	artifactPlace, []string, []string, error,
) {
	where := x.artifactPlaces()[0]
	first := ""
	place := func(lines []registry.LateValue) ([]string, error) {
		var out []string
		for _, v := range lines {
			raw, err := resolveArtifactValue("", v, "path", sc)
			if err != nil {
				return nil, err
			}
			if raw == "" {
				continue
			}
			pl, rel, err := x.placeArtifactPath(raw)
			if err != nil {
				return nil, err
			}
			if first == "" {
				first, where = raw, pl
			} else if pl.name != where.name {
				return nil, fmt.Errorf("%q is in %s and %q in %s; an upload's paths are all in one of the two (A77)",
					first, where.name, raw, pl.name)
			}
			if rel == "." {
				// The whole directory, less the hidden files, as `path: .`
				// is at compile time.
				rel = "**"
			}
			out = append(out, rel)
		}
		return out, nil
	}
	include, err := place(a.Late.Include)
	if err != nil {
		return where, nil, nil, err
	}
	exclude, err := place(a.Late.Exclude)
	if err != nil {
		return where, nil, nil, err
	}
	return where, include, append(exclude, a.Exclude...), nil
}

func artifactRefused(label string, err error) outcome {
	return outcome{state: protocol.StateFailed, code: protocol.ErrArtifact, msg: fmt.Sprintf("step %q: %v", label, err)}
}

// artifactPlace is one of the two directories an artifact step reaches: the
// workspace, where a step starts and where a relative path is, and the run's
// temporary directory, runner.temp (A77). seen is the directory as a step
// sees it, host as the agent reaches it; host is "" for a temporary directory
// the agent cannot reach.
type artifactPlace struct {
	name, seen, host string
	workspace        bool
}

// join is dest as a step names it, for download-path: relative in the
// workspace, as the compiler substitutes it when the path is known at
// admission, and absolute in runner.temp, which no step starts in.
func (p artifactPlace) join(dest string) string {
	if p.workspace {
		return dest
	}
	dir := p.seen
	if dir == "" {
		dir = p.host
	}
	return path.Join(filepath.ToSlash(dir), dest)
}

func (x *execution) artifactPlaces() [2]artifactPlace {
	ws := artifactPlace{name: "the workspace", host: x.ws.Path, workspace: true}
	temp := artifactPlace{name: "runner.temp"}
	if x.env != nil {
		ws.seen = x.env.WorkspaceDir()
		temp.seen = x.env.TempDir()
		if ht, ok := x.env.(executor.HostTemp); ok {
			temp.host = ht.HostTempDir()
		}
	}
	return [2]artifactPlace{ws, temp}
}

// placeArtifactPath answers which place p is in, and p relative to it.
//
// p is a path as the step that wrote it sees it: relative to the workspace,
// or absolute in the workspace or in runner.temp — the executor's path for
// either, or the host's, or the host's with its symlinks resolved, since on a
// Mac a temporary directory is reached through /var → /private/var and a step
// that printed `pwd -P` wrote the second. The longest match wins. Anything
// else is refused rather than collected: an artifact step copies between this
// run's store and those two directories, and nowhere else (A49, §17).
func (x *execution) placeArtifactPath(p string) (artifactPlace, string, error) {
	places := x.artifactPlaces()
	if strings.HasPrefix(p, "~") {
		return places[0], "", fmt.Errorf(
			"%q is in a home directory, and an artifact step reaches the workspace and runner.temp only (A77)", p)
	}
	if !strings.HasPrefix(p, "/") {
		for strings.HasPrefix(p, "./") {
			p = strings.TrimPrefix(p, "./")
		}
		if p == "" || p == "." {
			return places[0], ".", nil
		}
		return places[0], p, nil
	}

	clean := path.Clean(p)
	var found *artifactPlace
	bestLen, rel := -1, ""
	for i := range places {
		for _, dir := range candidates(places[i]) {
			r, ok := under(clean, dir)
			if ok && len(dir) > bestLen {
				found, bestLen, rel = &places[i], len(dir), r
			}
		}
	}
	if found == nil {
		return places[0], "", fmt.Errorf(
			"%q is outside the workspace and runner.temp, the two places an artifact step reaches (A77); "+
				"copy the file into one of them in an earlier step", p)
	}
	if found.host == "" {
		return *found, "", fmt.Errorf(
			"%q is in runner.temp, which on this runner is inside the container, where the agent cannot "+
				"reach it (A77); copy the file into the workspace in an earlier step, or run the job natively", p)
	}
	return *found, rel, nil
}

func candidates(pl artifactPlace) []string {
	var out []string
	for _, d := range []string{pl.seen, pl.host} {
		if d == "" {
			continue
		}
		out = append(out, path.Clean(filepath.ToSlash(d)))
		if real, err := filepath.EvalSymlinks(d); err == nil {
			out = append(out, path.Clean(filepath.ToSlash(real)))
		}
	}
	return out
}

// under is p relative to dir, "." for dir itself.
func under(p, dir string) (string, bool) {
	if p == dir {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(p, dir+"/"); ok {
		return rest, true
	}
	return "", false
}
