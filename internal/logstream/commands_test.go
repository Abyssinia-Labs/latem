package logstream

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

type memSink struct{ lines []string }

func (m *memSink) Write(frames []protocol.LogFrame) error {
	for _, f := range frames {
		m.lines = append(m.lines, string(f.Stream)+": "+f.Line)
	}
	return nil
}
func (m *memSink) Close() error { return nil }

func newTestEmitter() (*Emitter, *memSink) {
	sink := &memSink{}
	return NewEmitter("r_1", NewRedactor(), sink), sink
}

// ::add-mask:: is INV-4 reached from inside a step. The value is redacted from
// that line on, and the line that carried it is never emitted.
func TestAddMaskRegistersAndIsNeverEmitted(t *testing.T) {
	e, sink := newTestEmitter()
	e.Emit(protocol.StreamStdout, "token is hunter2-secret before")
	e.Emit(protocol.StreamStdout, "::add-mask::hunter2-secret")
	e.Emit(protocol.StreamStdout, "token is hunter2-secret after")

	joined := strings.Join(sink.lines, "\n")
	if !strings.Contains(sink.lines[0], "hunter2-secret") {
		t.Errorf("before registration the value is ordinary output: %q", sink.lines[0])
	}
	if strings.Count(joined, "hunter2-secret") != 1 {
		t.Errorf("the value appears %d times; want once, from before the mask:\n%s",
			strings.Count(joined, "hunter2-secret"), joined)
	}
	if !strings.Contains(joined, "registered a value") {
		t.Errorf("the mask should leave a note that it happened:\n%s", joined)
	}
	if strings.Contains(sink.lines[len(sink.lines)-1], "hunter2-secret") {
		t.Errorf("after registration the value must be redacted: %q", sink.lines[len(sink.lines)-1])
	}
}

// Output that has the shape of a command but is not one the runner knows is
// output. The compiler error the developer is reading survives.
func TestUnknownCommandShapedOutputPassesThrough(t *testing.T) {
	e, sink := newTestEmitter()
	e.Emit(protocol.StreamStderr, "::std::vector<int> has no member named size_")
	if len(sink.lines) != 1 || !strings.Contains(sink.lines[0], "std::vector<int>") {
		t.Errorf("a C++ diagnostic was eaten: %v", sink.lines)
	}
}

// stop-commands is how a workflow protects itself from output it does not
// trust: until the token line arrives, nothing is a command.
func TestStopCommandsPausesUntilTheToken(t *testing.T) {
	e, sink := newTestEmitter()
	e.Emit(protocol.StreamStdout, "::stop-commands::tok123")
	e.Emit(protocol.StreamStdout, "::add-mask::should-not-register")
	e.Emit(protocol.StreamStdout, "::tok123::")
	e.Emit(protocol.StreamStdout, "::add-mask::does-register")
	e.Emit(protocol.StreamStdout, "should-not-register does-register")

	last := sink.lines[len(sink.lines)-1]
	if !strings.Contains(last, "should-not-register") {
		t.Errorf("a mask issued while paused must not have taken effect: %q", last)
	}
	if strings.Contains(last, "does-register") {
		t.Errorf("a mask issued after resuming must take effect: %q", last)
	}
	// And the paused mask line was ordinary output, so its text is in the log.
	if !strings.Contains(strings.Join(sink.lines, "\n"), "::add-mask::should-not-register") {
		t.Error("while paused, a command line is plain output and should appear as such")
	}
}

// Deprecated stdout-based commands are refused for the reason GitHub
// deprecated them, and the note never carries the value that was sent.
func TestDeprecatedCommandsAreRefusedWithoutQuoting(t *testing.T) {
	e, sink := newTestEmitter()
	e.Emit(protocol.StreamStdout, "::set-output name=sha::deadbeef-value")
	joined := strings.Join(sink.lines, "\n")
	if !strings.Contains(joined, "deprecated") || !strings.Contains(joined, "GITHUB_OUTPUT") {
		t.Errorf("want a note naming the file to use instead:\n%s", joined)
	}
	if strings.Contains(joined, "deadbeef-value") {
		t.Errorf("the note quoted the value:\n%s", joined)
	}
}

func TestAnnotationsAndGroups(t *testing.T) {
	e, sink := newTestEmitter()
	e.Emit(protocol.StreamStdout, "::group::Build phase")
	e.Emit(protocol.StreamStdout, "::error file=a.go,line=12::it broke")
	e.Emit(protocol.StreamStdout, "::endgroup::")
	joined := strings.Join(sink.lines, "\n")
	for _, want := range []string{"▶ Build phase", "error: it broke (file=a.go line=12)", "end of group"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// A command inside a suppressed private-key block is part of the key, not a
// command. The PEM guard runs first.
func TestCommandsInsideAPEMBlockAreSuppressed(t *testing.T) {
	e, sink := newTestEmitter()
	e.Emit(protocol.StreamStdout, "-----BEGIN PRIVATE KEY-----")
	e.Emit(protocol.StreamStdout, "::add-mask::inside-key")
	e.Emit(protocol.StreamStdout, "-----END PRIVATE KEY-----")
	e.Emit(protocol.StreamStdout, "inside-key visible?")
	joined := strings.Join(sink.lines, "\n")
	if strings.Contains(joined, "registered a value") {
		t.Errorf("a mask inside a key block must not run:\n%s", joined)
	}
	if !strings.Contains(sink.lines[len(sink.lines)-1], "inside-key visible?") {
		t.Errorf("nothing was registered, so the later line is plain: %q", sink.lines[len(sink.lines)-1])
	}
}
