package logstream

import (
	"fmt"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// command acts on a workflow command a step wrote to its output (A42 phase 2).
// Callers hold e.mu, and the caller has already established that the line is a
// command the runner knows — see [ghcmd.Command.Known] for why that gate is a
// separate question from the grammar.
//
// Every command's text that came from the step passes through the redactor
// before it is emitted, the same as any other line. The one exception is the
// line that *is* a secret: `::add-mask::` is never emitted at all, because its
// value is exactly what the redactor is being told to remove.
func (e *Emitter) command(stream protocol.Stream, cmd ghcmd.Command) protocol.LogFrame {
	switch cmd.Name {
	case "add-mask":
		// INV-4's mechanism, reached from inside a step: a token a build
		// discovers at run time is redacted from this line on by the same
		// path as a declared secret. Empty is a no-op rather than an error —
		// `::add-mask::$MAYBE_UNSET` is a common shape.
		if v := strings.TrimSpace(cmd.Value); v != "" {
			e.red.Register([]byte(v))
		}
		return e.emitLocked(protocol.StreamAgent,
			"[latem] ::add-mask:: registered a value with the redactor")

	case "stop-commands":
		// Until a line reading `::<token>::` arrives, nothing is a command.
		// This is how a workflow protects itself from output it does not
		// trust — a test that prints a PR title, say — issuing an add-mask or
		// an error annotation on its behalf.
		e.stopToken = strings.TrimSpace(cmd.Value)
		return e.emitLocked(protocol.StreamAgent, "[latem] workflow commands paused")

	case "group":
		return e.emitLocked(protocol.StreamAgent, "[latem] ▶ "+e.red.Redact(cmd.Value))
	case "endgroup":
		return e.emitLocked(protocol.StreamAgent, "[latem] ◀ end of group")

	case "error", "warning", "notice", "debug":
		return e.emitLocked(stream, e.red.Redact(annotate(cmd)))

	case "echo":
		// Toggles command echoing on GitHub. Commands here are always in the
		// log as their effect, so there is nothing to toggle.
		return protocol.LogFrame{}

	case "set-output", "set-env", "add-path", "save-state":
		// Deprecated on GitHub for the file-based channels, and refused here
		// for the reason GitHub deprecated them: a value arriving through
		// stdout is a value any library the step calls can forge. The note
		// names the file to use and never quotes what was sent.
		return e.emitLocked(protocol.StreamAgent, fmt.Sprintf(
			"[latem] ::%s:: is deprecated and was ignored; %s", cmd.Name, deprecatedAdvice[cmd.Name]))
	}
	return protocol.LogFrame{}
}

var deprecatedAdvice = map[string]string{
	"set-output": "append NAME=value to the file named by $GITHUB_OUTPUT",
	"set-env":    "append NAME=value to the file named by $GITHUB_ENV",
	"add-path":   "append the directory to the file named by $GITHUB_PATH",
	"save-state": "$GITHUB_STATE is not supported; carry state through $GITHUB_ENV",
}

// annotate renders an error/warning/notice/debug command the way a reader
// expects: the level, the message, and the location parameters if given.
func annotate(cmd ghcmd.Command) string {
	var b strings.Builder
	b.WriteString(cmd.Name + ": " + cmd.Value)
	var loc []string
	for _, k := range []string{"file", "line", "col", "title"} {
		if v := cmd.Param(k); v != "" {
			loc = append(loc, k+"="+v)
		}
	}
	if len(loc) > 0 {
		b.WriteString(" (" + strings.Join(loc, " ") + ")")
	}
	return b.String()
}

// resumesCommands reports whether a line ends a stop-commands pause. Callers
// hold e.mu.
func (e *Emitter) resumesCommands(line string) bool {
	if e.stopToken == "" {
		return false
	}
	trimmed := strings.TrimRight(line, "\r\n")
	return trimmed == "::"+e.stopToken+"::"
}
