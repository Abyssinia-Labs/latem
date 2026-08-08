package protocol

import "errors"

// ErrorCode is a stable identifier for why a run did not succeed. The web app
// maps these to human copy, so the strings are API and renaming one is a
// breaking change (RUNNER_SPEC §21).
type ErrorCode string

const (
	// Admission failures — the run never started.
	ErrUnknownRepo   ErrorCode = "E_UNKNOWN_REPO"
	ErrInvalidRef    ErrorCode = "E_INVALID_REF"
	ErrAmbiguousRef  ErrorCode = "E_AMBIGUOUS_REF"
	ErrUnknownJob    ErrorCode = "E_UNKNOWN_JOB"
	ErrNoRunner      ErrorCode = "E_NO_RUNNER"
	ErrBadInput      ErrorCode = "E_BAD_INPUT"
	ErrMissingSecret ErrorCode = "E_MISSING_SECRET"

	// Registry failures — the commit is checked out but unusable.
	ErrNoRegistry  ErrorCode = "E_NO_REGISTRY"
	ErrBadRegistry ErrorCode = "E_BAD_REGISTRY"

	// Environment failures — generally worth retrying.
	ErrSource   ErrorCode = "E_SOURCE"
	ErrExecutor ErrorCode = "E_EXECUTOR"
	ErrCache    ErrorCode = "E_CACHE"
	ErrDisk     ErrorCode = "E_DISK"

	// Execution outcomes.
	ErrStep        ErrorCode = "E_STEP"
	ErrTimeout     ErrorCode = "E_TIMEOUT"
	ErrStepTimeout ErrorCode = "E_STEP_TIMEOUT"
	ErrArtifact    ErrorCode = "E_ARTIFACT"
	ErrInterrupted ErrorCode = "E_INTERRUPTED"

	// Expiries — nobody acted in time.
	ErrApprovalExpired ErrorCode = "E_APPROVAL_EXPIRED"
	ErrQueueExpired    ErrorCode = "E_QUEUE_EXPIRED"

	// ErrSecretBackend means a secret store is unusable, as distinct from a
	// secret nobody configured: a locked keychain, `op` not signed in, a
	// world-readable secrets.env. It is retryable because the most common
	// instance — a Mac mini whose keychain is locked after a reboot — is a
	// machine condition that resolves without anyone editing a commit
	// (amendment A16).
	ErrSecretBackend ErrorCode = "E_SECRET_BACKEND"

	// ErrAgentDown is a CLI condition, not a run outcome, and never appears in
	// [Update.ErrorCode]. `latem run --wait` exits 3 with this when no agent is
	// listening (RUNNER_SPEC §7.3). It is declared here so the CLI and any
	// tooling reading its output share one spelling.
	ErrAgentDown ErrorCode = "E_AGENT_DOWN"
)

// ErrNoAgent is the sentinel behind [ErrAgentDown], for errors.Is.
//
// The code names the condition for a human; this names it for a program. Both
// the local broker's client and the CLI need to recognise "nothing is
// listening" and each had defined its own value, which is two spellings of one
// condition waiting to disagree (amendment A20).
//
// The exit code stays in the CLI rather than here: §7.3 fixes it at 3, but an
// exit status is a property of a process, and this package is the wire.
var ErrNoAgent = errors.New("latem: no agent is listening on the local socket")

// errorInfo is the single source for the §21 table. Deriving both the terminal
// state and the retry hint from one map is deliberate: two independent switch
// statements over twenty codes drift, and the drift is invisible until a
// control plane offers a retry button for something that cannot succeed.
type errorInfo struct {
	state     RunState
	retryable bool
}

var errorTable = map[ErrorCode]errorInfo{
	ErrUnknownRepo:     {StateRejected, false},
	ErrInvalidRef:      {StateRejected, false},
	ErrAmbiguousRef:    {StateRejected, false},
	ErrUnknownJob:      {StateRejected, false},
	ErrNoRunner:        {StateRejected, false},
	ErrBadInput:        {StateRejected, false},
	ErrMissingSecret:   {StateRejected, false},
	ErrApprovalExpired: {StateRejected, false},
	ErrQueueExpired:    {StateRejected, false},

	ErrNoRegistry:  {StateFailed, false},
	ErrBadRegistry: {StateFailed, false},
	ErrStep:        {StateFailed, false},
	ErrArtifact:    {StateFailed, false},

	ErrSource:        {StateFailed, true},
	ErrExecutor:      {StateFailed, true},
	ErrCache:         {StateFailed, true},
	ErrDisk:          {StateFailed, true},
	ErrSecretBackend: {StateFailed, true},

	ErrTimeout:     {StateTimedOut, false},
	ErrStepTimeout: {StateTimedOut, false},

	ErrInterrupted: {StateInterrupted, true},
}

// Retryable reports whether a control plane may offer this as a one-click
// retry. Non-retryable codes need a human or a commit, and offering a retry
// for one is a worse experience than not offering it at all.
//
// [ErrAgentDown] is not a run outcome and reports false.
func (c ErrorCode) Retryable() bool { return errorTable[c].retryable }

// TerminalState returns the state a run reaches when it fails with this code.
// The zero RunState is returned for codes that are not run outcomes.
func (c ErrorCode) TerminalState() RunState { return errorTable[c].state }

// Valid reports whether c is a known run-outcome code.
func (c ErrorCode) Valid() bool {
	_, ok := errorTable[c]
	return ok
}

func (c ErrorCode) String() string { return string(c) }
