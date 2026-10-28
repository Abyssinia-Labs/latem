package native

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// EnvFileName is the basename of the file exported as $LATEM_ENV. It lives
	// in a per-run directory under [Options.EnvFileDir], never in the
	// workspace: the workspace is a git clone (§12), and a file appearing in it
	// would show up in `git status`, in `git clean -n`, and eventually in
	// somebody's commit.
	EnvFileName = "env"
	// OutputFileName is the basename of the file exported as $GITHUB_OUTPUT,
	// beside the env file. Created and truncated the same way; unlike the env
	// file, never carried, because outputs belong to the step that wrote
	// them.
	OutputFileName = "output"
	// PathFileName and SummaryFileName are the last two of the four, exported
	// as $GITHUB_PATH and $GITHUB_STEP_SUMMARY.
	PathFileName    = "path"
	SummaryFileName = "summary"
	// StateFileName is the basename of the file exported as $GITHUB_STATE
	// (A51), beside the four above and handled like $GITHUB_OUTPUT.
	StateFileName = "state"
	// TempDirName is the basename of the directory exported as $RUNNER_TEMP
	// (A50), beside the files above so that removing the run's directory
	// empties it.
	TempDirName = "temp"

	// MaxEnvFileBytes is §9's 64 KB cap per step. The file is job-authored data
	// the agent then trusts, so it gets a size it cannot exceed as well as a
	// grammar it cannot break.
	MaxEnvFileBytes = 64 << 10

	// envDirMode and envFileMode are tight because, unlike the docker executor,
	// this package knows exactly who will be reading and writing: the same user
	// the agent runs as. There is no uid to guess at, so there is no reason to
	// widen them. The file can hold whatever a step chose to put in it, which
	// on a signing runner is occasionally something derived from a credential.
	envDirMode  = 0o700
	envFileMode = 0o600
)

// parseEnvFile parses the KEY=value lines a step appended to $LATEM_ENV.
//
// The constraints come from RUNNER_SPEC §9 and each one exists because this is
// a file a job writes and the agent then believes:
//
//   - Keys must be identifiers. Anything else is dropped.
//   - Keys the protected predicate rejects are ignored. That covers every
//     LATEM_* name, so a job cannot spoof the agent's own variables, and every
//     declared secret name, so a job cannot overwrite an injected credential.
//   - The file is capped at [MaxEnvFileBytes]; past the cap the tail is
//     discarded, along with the partial line at the cut so that half a value
//     never becomes a whole one.
//   - Nothing is dropped silently. A job author whose `echo` did not take
//     effect gets told why in the log of the step that wrote it.
//
// Warnings never quote the line or the value, only the position and, where the
// key is known to be a well-formed identifier, the key. A malformed line may be
// anything at all — including a secret a script printed into the wrong file
// descriptor — and echoing it into the log to explain that it was dropped would
// be its own leak.
//
// This is deliberately the same parser as the docker executor's, down to the
// warning text. §9's contract is "identical across all three executors", and
// two implementations that agree today drift the first time one of them fixes a
// bug; see the package doc for why the shared version cannot live in
// internal/executor.
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
// Two rules. Any LATEM_* name is the agent's, so a job cannot spoof $LATEM_ENV
// itself or any variable the agent later adds.
//
// Any declared secret name is refused, which is exactly what §9 asks for.
// [executor.Spec.Secrets] names them (amendment A10); before it existed this
// package could not tell MATCH_PASSWORD from NODE_ENV and had to refuse the
// whole injected set.
//
// The fallback survives on purpose. An executor handed a non-empty Env with no
// Secrets list has no way to identify credentials, so it protects everything
// injected. Failing open there would permit a credential overwrite; failing
// closed costs a confusing refusal of a legal override, and that is the cheaper
// mistake.
func protectedEnvKey(runEnvironment map[string]string, secrets []string, key string) bool {
	if strings.HasPrefix(key, EnvPrefix) {
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
// Reading and truncating are two operations rather than one atomic one, which
// is fine: steps do not overlap, so nothing is writing to the file between
// them. That is a consequence of §9's model, not an assumption this package
// adds.
func (e *runEnv) mergeEnvFile(out io.Writer) error {
	data, err := e.readEnvFile()
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
		if err := e.createEnvFile(); err != nil {
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

// readEnvFile reads $LATEM_ENV from disk.
//
// A missing file is not an error: most steps never touch $LATEM_ENV, and a step
// is free to delete it, so "absent" and "empty" both mean the same thing —
// nothing to merge.
func (e *runEnv) readEnvFile() ([]byte, error) { return e.readBack(e.envFile, "$LATEM_ENV") }

// readBack reads one of the per-run bookkeeping files a step may have written.
func (e *runEnv) readBack(file, label string) ([]byte, error) {
	// #nosec G304 -- file is built in Prepare from Options.EnvFileDir and a
	// run id already through protocol.ValidateID (amendment A6). No part of it
	// is broker-supplied or job-supplied.
	f, err := os.Open(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, wrapErr(protocol.ErrExecutor, err, "read %s (%s)", label, file)
	}
	defer func() { _ = f.Close() }()

	// One byte past the cap, so [parseEnvFile] can tell "exactly at the limit"
	// from "over it" and warn only in the second case. The limit is also what
	// stops a job filling the agent's memory from a `yes >> $LATEM_ENV` that
	// nothing else would bound — on native there is no container quota behind
	// it, only the host's disk.
	data, err := io.ReadAll(io.LimitReader(f, MaxEnvFileBytes+1))
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "read %s (%s)", label, file)
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// createEnvFile writes an empty $LATEM_ENV, creating its directory. It is both
// the initial creation and the between-step truncation, because a zero-length
// overwrite is exactly a truncation.
//
// O_TRUNC rather than remove-and-recreate so that a step which captured the
// path and holds it open across the boundary sees an empty file rather than a
// deleted inode it can keep appending to invisibly.
func (e *runEnv) createEnvFile() error { return e.createEmpty(e.envFile, "$LATEM_ENV") }

// createEmpty creates or truncates one bookkeeping file, creating the run's
// directory on the way.
func (e *runEnv) createEmpty(file, label string) error {
	if err := os.MkdirAll(e.envDir, envDirMode); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "create %s directory (%s)", label, e.envDir)
	}
	// #nosec G304 -- see readBack: the path is built from agent config and a
	// validated run id.
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, envFileMode)
	if err != nil {
		return wrapErr(protocol.ErrExecutor, err, "create %s (%s)", label, file)
	}
	if err := f.Close(); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "create %s (%s)", label, file)
	}
	return nil
}

// readOutputs reads and truncates $GITHUB_OUTPUT after a step.
//
// The parser is shared with the workflow package because the format is
// GitHub's, heredoc and all. There is no protected predicate: an output is a
// named value handed to later steps' expressions, not an environment entry,
// so there is nothing for it to shadow.
func (e *runEnv) readOutputs(out io.Writer) (map[string]string, error) {
	return e.readKeyValues(e.outFile, "$GITHUB_OUTPUT", out)
}

// readState reads and truncates $GITHUB_STATE after a step (A51).
func (e *runEnv) readState(out io.Writer) (map[string]string, error) {
	return e.readKeyValues(e.stateFile, "$GITHUB_STATE", out)
}

// readKeyValues reads and truncates one of the key=value files, in GitHub's
// format, heredoc and all.
func (e *runEnv) readKeyValues(file, label string, out io.Writer) (map[string]string, error) {
	data, err := e.readBack(file, label)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	assignments, warnings := ghcmd.ParseKeyValueFile(data, nil)
	for _, w := range warnings {
		writeNote(out, label+": "+w)
	}
	if err := e.createEmpty(file, label); err != nil {
		return nil, err
	}
	if len(assignments) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(assignments))
	for _, a := range assignments {
		values[a.Key] = a.Value
	}
	return values, nil
}

// readPathAndSummary reads and truncates $GITHUB_PATH and $GITHUB_STEP_SUMMARY
// after a step. PATH additions accumulate for the rest of the run; the summary
// goes to the step's log, one note per line, since there is nowhere else to
// render it.
func (e *runEnv) readPathAndSummary(out io.Writer) error {
	data, err := e.readBack(e.pathFile, "$GITHUB_PATH")
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
		if err := e.createEmpty(e.pathFile, "$GITHUB_PATH"); err != nil {
			return err
		}
	}

	data, err = e.readBack(e.summaryFile, "$GITHUB_STEP_SUMMARY")
	if err != nil {
		return err
	}
	if len(data) > 0 {
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			writeNote(out, "summary: "+line)
		}
		if err := e.createEmpty(e.summaryFile, "$GITHUB_STEP_SUMMARY"); err != nil {
			return err
		}
	}
	return nil
}
