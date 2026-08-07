package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDuration_AcceptsGoStrings(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30m":   30 * time.Minute,
		"1h30m": 90 * time.Minute,
		"90s":   90 * time.Second,
		"0s":    0,
		"336h":  14 * 24 * time.Hour,
	} {
		var d Duration
		if err := json.Unmarshal([]byte(`"`+in+`"`), &d); err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if d.Duration() != want {
			t.Errorf("%q decoded to %v, want %v", in, d.Duration(), want)
		}
	}
}

// A bare number is ambiguous: Go reads 30 as 30 nanoseconds, every human
// writing `timeout: 30` in a CI file means minutes. Refusing beats guessing.
func TestDuration_RejectsBareNumbers(t *testing.T) {
	for _, in := range []string{`30`, `0`, `1.5`, `null`, `true`, `["30m"]`} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("accepted %s as a duration", in)
		}
	}
}

func TestDuration_RejectsGarbage(t *testing.T) {
	for _, in := range []string{`"30 minutes"`, `"thirty"`, `""`, `"30x"`} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("accepted %s as a duration", in)
		}
	}
}

// UnmarshalText is the path BurntSushi/toml takes, so config.toml depends on it
// working independently of the JSON path.
func TestDuration_TextRoundTrip(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("45m")); err != nil {
		t.Fatal(err)
	}
	if d.Duration() != 45*time.Minute {
		t.Fatalf("got %v", d.Duration())
	}
	b, err := d.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "45m0s" {
		t.Errorf("marshalled to %q", b)
	}
}

// The terminal set here and the partial index in RUNNER_SPEC §22 are the same
// set written twice. This pins the Go half.
func TestRunState_TerminalSet(t *testing.T) {
	terminal := []RunState{
		StateSucceeded, StateFailed, StateTimedOut,
		StateCancelled, StateRejected, StateInterrupted,
	}
	nonTerminal := []RunState{
		StateQueued, StateAwaitingApproval, StateClaimed,
		StatePreparing, StateRunning, StateCollecting,
	}

	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
	if got := len(TerminalStates()); got != len(terminal) {
		t.Errorf("TerminalStates returned %d entries, want %d", got, len(terminal))
	}
	for _, s := range append(terminal, nonTerminal...) {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if RunState("running_fast").Valid() {
		t.Error("unknown state reported valid")
	}
}

// Only these two states park a run pending something external, and they are
// exactly the ones carrying an expiry deadline.
func TestRunState_Waiting(t *testing.T) {
	if !StateQueued.IsWaiting() || !StateAwaitingApproval.IsWaiting() {
		t.Error("queued and awaiting_approval must be waiting states")
	}
	for _, s := range []RunState{StateClaimed, StateRunning, StateSucceeded} {
		if s.IsWaiting() {
			t.Errorf("%s must not be a waiting state", s)
		}
	}
}

// Retryability is what a control plane hangs a one-click retry button on.
// Offering it for something that cannot succeed is worse than not offering it.
func TestErrorCode_RetryableMatchesSpec(t *testing.T) {
	retryable := []ErrorCode{ErrSource, ErrExecutor, ErrCache, ErrDisk, ErrInterrupted}
	notRetryable := []ErrorCode{
		ErrUnknownRepo, ErrInvalidRef, ErrAmbiguousRef, ErrUnknownJob,
		ErrNoRunner, ErrBadInput, ErrMissingSecret, ErrNoRegistry,
		ErrBadRegistry, ErrStep, ErrTimeout, ErrStepTimeout, ErrArtifact,
		ErrApprovalExpired, ErrQueueExpired,
	}

	for _, c := range retryable {
		if !c.Retryable() {
			t.Errorf("%s should be retryable", c)
		}
	}
	for _, c := range notRetryable {
		if c.Retryable() {
			t.Errorf("%s should not be retryable", c)
		}
	}
}

func TestErrorCode_TerminalStates(t *testing.T) {
	for code, want := range map[ErrorCode]RunState{
		ErrUnknownRepo:     StateRejected,
		ErrApprovalExpired: StateRejected,
		ErrQueueExpired:    StateRejected,
		ErrStep:            StateFailed,
		ErrSource:          StateFailed,
		ErrTimeout:         StateTimedOut,
		ErrStepTimeout:     StateTimedOut,
		ErrInterrupted:     StateInterrupted,
	} {
		if got := code.TerminalState(); got != want {
			t.Errorf("%s → %s, want %s", code, got, want)
		}
		if !code.TerminalState().IsTerminal() {
			t.Errorf("%s maps to a non-terminal state", code)
		}
	}
}

// E_AGENT_DOWN is a CLI exit condition, not a run outcome, and must never
// appear in Update.ErrorCode.
func TestErrorCode_AgentDownIsNotARunOutcome(t *testing.T) {
	if ErrAgentDown.Valid() {
		t.Error("E_AGENT_DOWN must not be a run-outcome code")
	}
	if ErrAgentDown.Retryable() {
		t.Error("E_AGENT_DOWN must not be retryable")
	}
}
