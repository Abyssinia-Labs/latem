package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// EnvFilePath is the container-visible path exported as $LATEM_ENV, and
	// the value the agent should put in [executor.Step.EnvFile].
	//
	// It lives inside the container rather than on the bind-mounted workspace
	// on purpose: the workspace is a git clone (§12), and a file appearing in
	// it would show up in `git status`, in `git clean -n`, and eventually in
	// somebody's commit.
	EnvFilePath = "/tmp/latem/env"
	// OutputFilePath is the container-visible path exported as $GITHUB_OUTPUT.
	// A sibling of the env file, created and truncated the same way, and
	// unlike it not carried: outputs belong to the step that wrote them.
	OutputFilePath = "/tmp/latem/output"
	// PathFilePath and SummaryFilePath are the last two of the four, exported
	// as $GITHUB_PATH and $GITHUB_STEP_SUMMARY.
	PathFilePath    = "/tmp/latem/path"
	SummaryFilePath = "/tmp/latem/summary"

	// MaxEnvFileBytes is §9's 64 KB cap per step. The file is job-authored
	// data the agent then trusts, so it gets a size it cannot exceed as well
	// as a grammar it cannot break.
	MaxEnvFileBytes = 64 << 10

	// envFileMode and envDirMode are permissive because this package cannot
	// know which uid the image's `runner` maps to at the time it creates the
	// file, and a $LATEM_ENV the job cannot append to is a silently broken
	// feature. The blast radius is one file in one single-tenant container
	// that is destroyed at the end of the run.
	envFileMode = 0o666
	envDirMode  = 0o777
)

// parseEnvFile parses the KEY=value lines a step appended to $LATEM_ENV.
//
// The constraints come from RUNNER_SPEC §9 and each one exists because this is
// a file a job writes and the agent then believes:
//
//   - Keys must be identifiers. Anything else is dropped.
//   - Keys the protected predicate rejects are ignored. That covers every
//     LATEM_* name, so a job cannot spoof the agent's own variables, and every
//     name already in the run environment, so a job cannot overwrite an
//     injected credential.
//   - The file is capped at [MaxEnvFileBytes]; past the cap the tail is
//     discarded, along with the partial line at the cut so that half a value
//     never becomes a whole one.
//   - Nothing is dropped silently. A job author whose `echo` did not take
//     effect gets told why in the log of the step that wrote it.
//
// Warnings never quote the line or the value, only the position and, where the
// key is known to be a well-formed identifier, the key. A malformed line may
// be anything at all — including a secret a script printed into the wrong file
// descriptor — and echoing it into the log to explain that it was dropped
// would be its own leak.
func parseEnvFile(data []byte, protected func(string) bool) (map[string]string, []string) {
	// The parser is the workflow package's since A43: the format is GitHub's
	// $GITHUB_ENV format, which this file is exported as too, and one parser
	// for one format is the whole point. Its cap is the same 64 KB, its key
	// grammar is §9's, and it never quotes a value or a malformed line. What
	// this adapter adds is the merge — last writer wins, as before — and the
	// file's name on each warning, so a reader knows which channel spoke.
	assignments, warnings := ghcmd.ParseKeyValueFile(data, protected)
	for i, w := range warnings {
		warnings[i] = "$LATEM_ENV " + w
	}
	out := make(map[string]string, len(assignments))
	for _, a := range assignments {
		out[a.Key] = a.Value
	}
	return out, warnings
}

// protectedEnvKey reports whether a job is forbidden from setting key.
//
// Two rules. Any LATEM_* name is the agent's, so a job cannot spoof
// $LATEM_ENV itself or any variable the agent later adds.
//
// Any declared secret name is refused, which is exactly what §9 asks for.
// [executor.Spec.Secrets] names them (amendment A10); before it existed this
// package could not tell MATCH_PASSWORD from NODE_ENV and had to refuse the
// whole injected set.
//
// The fallback survives on purpose. An executor handed a non-empty Env with no
// Secrets list has no way to identify credentials, so it protects everything
// injected. Failing open there would permit a credential overwrite; failing
// closed costs a confusing refusal of a legal override, and that is the
// cheaper mistake.
func protectedEnvKey(runEnvironment map[string]string, secrets []string, key string) bool {
	if strings.HasPrefix(key, "LATEM_") {
		return true
	}
	if len(secrets) > 0 {
		for _, name := range secrets {
			if name == key {
				return true
			}
		}
		return false
	}
	_, injected := runEnvironment[key]
	return injected
}

// mergeEnvFile reads, truncates and merges $LATEM_ENV after a step.
//
// Reading and truncating are two API calls rather than one atomic operation,
// which is fine: steps do not overlap, so nothing is writing to the file
// between them. That is a consequence of §9's model, not an assumption this
// package adds.
func (e *runEnv) mergeEnvFile(ctx context.Context, envFile string, out io.Writer) error {
	data, err := e.readEnvFile(ctx, envFile)
	if err != nil {
		return err
	}
	if data == nil {
		return nil
	}

	vars, warnings := parseEnvFile(data, func(k string) bool {
		return protectedEnvKey(e.spec.Env, e.spec.Secrets, k)
	})
	for _, w := range warnings {
		writeNote(out, w)
	}

	if len(data) > 0 {
		if err := e.createEnvFile(ctx, envFile); err != nil {
			return err
		}
	}

	e.mu.Lock()
	for k, v := range vars {
		e.carried[k] = v
	}
	e.mu.Unlock()
	return nil
}

// writeNote emits an agent-authored line into the step's output stream. The
// prefix matches logstream's, so a reader sees one convention for "this line
// came from latem, not from your build".
func writeNote(out io.Writer, msg string) {
	if out == nil {
		return
	}
	_, _ = io.WriteString(out, "[latem] "+msg+"\n")
}

// readEnvFile copies $LATEM_ENV out of the container.
//
// A missing file is not an error: most steps never touch $LATEM_ENV, and the
// file is recreated empty after every step that does, so "absent" and "empty"
// both mean the same thing — nothing to merge.
func (e *runEnv) readEnvFile(ctx context.Context, envFile string) ([]byte, error) {
	rc, _, err := e.api.CopyFromContainer(ctx, e.containerID, envFile)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, wrapErr(protocol.ErrExecutor, err, "read $LATEM_ENV (%s)", envFile)
	}
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "read $LATEM_ENV (%s)", envFile)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, nil
	}

	// One byte past the cap, so [parseEnvFile] can tell "exactly at the limit"
	// from "over it" and warn only in the second case. The limit is also what
	// stops a job filling the agent's memory from inside a container.
	data, err := io.ReadAll(io.LimitReader(tr, MaxEnvFileBytes+1))
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "read $LATEM_ENV (%s)", envFile)
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// createEnvFile writes an empty $LATEM_ENV into the container, creating its
// directory. It is both the initial creation and the between-step truncation,
// because a zero-length overwrite is exactly a truncation.
//
// It uses the archive API rather than an exec so that it works regardless of
// what the image contains, and so that the file's mode is set by this package
// instead of by whatever umask the image ships.
func (e *runEnv) createEnvFile(ctx context.Context, envFile string) error {
	dir := path.Dir(envFile)
	parent := path.Dir(dir)
	dirName := path.Base(dir)
	fileName := path.Base(envFile)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     dirName + "/",
		Typeflag: tar.TypeDir,
		Mode:     envDirMode,
	}); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "build $LATEM_ENV archive")
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     dirName + "/" + fileName,
		Typeflag: tar.TypeReg,
		Mode:     envFileMode,
		Size:     0,
	}); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "build $LATEM_ENV archive")
	}
	if err := tw.Close(); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "build $LATEM_ENV archive")
	}

	if err := e.api.CopyToContainer(ctx, e.containerID, parent, &buf,
		container.CopyToContainerOptions{}); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "create $LATEM_ENV (%s)", envFile)
	}
	return nil
}

// readOutputs reads and truncates $GITHUB_OUTPUT after a step.
//
// The parser is shared with the workflow package because the format is
// GitHub's, heredoc and all. There is no protected predicate: an output is a
// named value handed to later steps' expressions, not an environment entry,
// so there is nothing for it to shadow.
func (e *runEnv) readOutputs(ctx context.Context, out io.Writer) (map[string]string, error) {
	data, err := e.readEnvFile(ctx, OutputFilePath)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	assignments, warnings := ghcmd.ParseKeyValueFile(data, nil)
	for _, w := range warnings {
		writeNote(out, "$GITHUB_OUTPUT: "+w)
	}
	if err := e.createEnvFile(ctx, OutputFilePath); err != nil {
		return nil, err
	}
	if len(assignments) == 0 {
		return nil, nil
	}
	outputs := make(map[string]string, len(assignments))
	for _, a := range assignments {
		outputs[a.Key] = a.Value
	}
	return outputs, nil
}

// readPathAndSummary reads and truncates $GITHUB_PATH and $GITHUB_STEP_SUMMARY
// after a step. PATH additions accumulate for the rest of the run; the summary
// goes to the step's log, one note per line, since there is nowhere else to
// render it.
func (e *runEnv) readPathAndSummary(ctx context.Context, out io.Writer) error {
	data, err := e.readEnvFile(ctx, PathFilePath)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		dirs, warnings := ghcmd.ParsePathFile(data)
		for _, w := range warnings {
			writeNote(out, "$GITHUB_PATH: "+w)
		}
		e.mu.Lock()
		e.pathAdds = ghcmd.AccumulatePath(e.pathAdds, dirs)
		e.mu.Unlock()
		if err := e.createEnvFile(ctx, PathFilePath); err != nil {
			return err
		}
	}

	data, err = e.readEnvFile(ctx, SummaryFilePath)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			writeNote(out, "summary: "+line)
		}
		if err := e.createEnvFile(ctx, SummaryFilePath); err != nil {
			return err
		}
	}
	return nil
}
