package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// CommandKind names an inbound verb. The set is closed at three values.
//
// Closing it is the mechanical half of INV-2: a control plane can ask the
// agent to start work it already knows how to do, to stop work it already
// started, or to say what an event would start (A52), and there is nothing
// else it can say. Adding a kind requires a written threat model and a change
// to RUNNER_SPEC §5 — it is not a feature.
type CommandKind string

const (
	// CommandRun asks the agent to execute a named job. See [RunRequest].
	CommandRun CommandKind = "run"
	// CommandCancel asks the agent to stop a run it owns. See [CancelRequest].
	CommandCancel CommandKind = "cancel"
	// CommandPlan asks the agent which workflow runs an event starts. It
	// reads files and runs nothing. See [PlanRequest].
	CommandPlan CommandKind = "plan"
)

// MaxCancelReasonRunes bounds [CancelRequest.Reason]. The field is display
// text originating outside the trust boundary; the cap stops it being used as
// a channel for bulk data or as a way to blow up a log line.
const MaxCancelReasonRunes = 200

// MaxIDLength bounds a run identifier.
const MaxIDLength = 64

// idPattern is deliberately narrower than "any string". See [ValidateID].
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateID checks a run identifier from outside the trust boundary.
//
// A run id is not merely a label. The agent builds filesystem paths from it —
// work/<id>, logs/<id>.log, artifacts/<id>/ — and container and VM names,
// latem-run-<id>. An id containing "/" or ".." is therefore a path traversal
// with a control plane on the far end of it: `{"id": "../../../../tmp/x"}`
// gets a file written wherever the agent's user can write.
//
// RUNNER_SPEC's INV-3 enumerates validation for repo, ref, and inputs, and
// says nothing about id, which is how this was missed. The character class
// here excludes every separator and every dot, so no id can escape a
// directory or collide with "." and "..".
//
// Consumers should still keep their own defences — see logstream.NewFileSink —
// because a single validation point that everything else trusts is exactly the
// shape of the next incident.
func ValidateID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("protocol: run id is empty")
	case len(id) > MaxIDLength:
		return fmt.Errorf("protocol: run id is %d bytes, limit %d", len(id), MaxIDLength)
	case !idPattern.MatchString(id):
		return fmt.Errorf(
			"protocol: run id %q must match %s — ids become file and container names", id, idPattern)
	}
	return nil
}

// Command is one inbound message. Exactly one payload field is populated, and
// it is the one matching Kind.
type Command struct {
	Kind   CommandKind    `json:"kind"`
	Run    *RunRequest    `json:"run,omitempty"`
	Cancel *CancelRequest `json:"cancel,omitempty"`
	Plan   *PlanRequest   `json:"plan,omitempty"`
}

// RunRequest is the complete set of data crossing the trust boundary inbound
// for an execution.
//
// Every field is a name or an identifier. None of them can express a command,
// a script, an image, an environment, a file path, an executor, a cache
// configuration, a timeout, or a label override — the agent resolves all of
// that from a registry it read from its own disk at the resolved commit
// (INV-5). Adding a field here is a security review.
type RunRequest struct {
	ID     string            `json:"id"`
	Repo   string            `json:"repo"`             // must exist in local config
	Job    string            `json:"job"`              // must exist in the registry
	Ref    string            `json:"ref"`              // validated per INV-3
	Inputs map[string]string `json:"inputs,omitempty"` // declared inputs only
	Origin Origin            `json:"origin"`           // audit only

	// The fields below are a workflow run's job (A52), and are all set or all
	// absent: Workflow names the file — the only path an inbound field may
	// name, and only by pattern — and Job is a job's id in it.

	Workflow string `json:"workflow,omitempty"`
	// SHA is the commit to build. It must be reachable from Ref: the job
	// builds what the event named, not whatever the branch says by the time
	// the job starts.
	SHA string `json:"sha,omitempty"`
	// Event is what started the run, as data.
	Event *Event `json:"event,omitempty"`
	// Needs are the results and outputs of the jobs this one needs.
	Needs map[string]NeedResult `json:"needs,omitempty"`
	// WorkflowRunID, RunNumber and RunAttempt are the workflow run's, shared by
	// its jobs: github.run_id, github.run_number, github.run_attempt.
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
	RunNumber     int64  `json:"run_number,omitempty"`
	RunAttempt    int64  `json:"run_attempt,omitempty"`
	// Token is a GitHub installation token minted for this job alone, scoped
	// by its permissions. The agent holds it in memory only — it is stripped
	// before the request is made durable — and registers it with the
	// redactor before the first step (A52, §12 as narrowed).
	Token string `json:"token,omitempty"`
	// Matrix is a matrix leg's values, a JSON object, as the plan reported
	// them (A55, 2.1). Data: the machine builds the leg only if it is one its
	// own expansion of the pinned commit's matrix yields.
	Matrix json.RawMessage `json:"matrix,omitempty"`
}

// CancelRequest carries an identifier and nothing executable.
//
// It exists because RUNNER_SPEC §11 defines a cancelled state reachable by
// broker cancellation, and because the hosted product promises a Stop button
// on a phone. Its entire effect is to cancel the context of work the agent
// itself started; it cannot name a job, a ref, a command, or a machine.
type CancelRequest struct {
	ID    string `json:"id"`
	Actor string `json:"actor,omitempty"`
	// Reason is display only. It is truncated to [MaxCancelReasonRunes] on
	// decode, never interpolated, never passed to a shell, and passes through
	// the redactor before it is logged or persisted (§16).
	Reason string `json:"reason,omitempty"`
}

// Origin records who asked, for audit and display.
//
// Nothing here is trusted. RequestedAt in particular is a timestamp from
// another machine's clock and must not be used for ordering, expiry, or any
// other decision — the agent stamps its own times (§22).
type Origin struct {
	Kind        string    `json:"kind"` // "manual" | "webhook" | "schedule" | "cli"
	Actor       string    `json:"actor,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

// NewRunCommand builds a run command. Used by the local broker, which
// constructs commands directly rather than decoding them.
func NewRunCommand(r RunRequest) Command {
	return Command{Kind: CommandRun, Run: &r}
}

// NewPlanCommand builds a plan command.
func NewPlanCommand(p PlanRequest) Command {
	return Command{Kind: CommandPlan, Plan: &p}
}

// NewCancelCommand builds a cancel command with its reason already normalized.
func NewCancelCommand(c CancelRequest) Command {
	c.Normalize()
	return Command{Kind: CommandCancel, Cancel: &c}
}

// Normalize applies the bounds this type promises. It is called on decode and
// by [NewCancelCommand], so no path can produce an over-long reason.
func (c *CancelRequest) Normalize() {
	if utf8.RuneCountInString(c.Reason) > MaxCancelReasonRunes {
		runes := []rune(c.Reason)
		c.Reason = string(runes[:MaxCancelReasonRunes])
	}
}

// Validate checks structural well-formedness: that the kind is known, that
// the payload matches it, and that an identifier is present.
//
// It deliberately does not check anything semantic. Whether Repo appears in
// local config, whether Ref survives the INV-3 regex, whether Job exists in
// the registry, and whether every input is declared are decided by
// internal/registry against state this package cannot see, as the ordered
// gate chain in RUNNER_SPEC §10. A Command that validates here is
// well-formed, not safe.
func (c Command) Validate() error {
	// other names a payload that does not belong to the kind, so a refusal
	// says which one.
	other := func(kind CommandKind) string {
		for _, p := range []struct {
			kind CommandKind
			set  bool
		}{{CommandRun, c.Run != nil}, {CommandCancel, c.Cancel != nil}, {CommandPlan, c.Plan != nil}} {
			if p.set && p.kind != kind {
				return string(p.kind)
			}
		}
		return ""
	}
	switch c.Kind {
	case CommandRun:
		if c.Run == nil {
			return fmt.Errorf("protocol: command kind %q with no run payload", c.Kind)
		}
		if o := other(c.Kind); o != "" {
			return fmt.Errorf("protocol: command kind %q carries a %s payload", c.Kind, o)
		}
		if err := ValidateID(c.Run.ID); err != nil {
			return err
		}
		return c.Run.validateWorkflowRun()

	case CommandCancel:
		if c.Cancel == nil {
			return fmt.Errorf("protocol: command kind %q with no cancel payload", c.Kind)
		}
		if o := other(c.Kind); o != "" {
			return fmt.Errorf("protocol: command kind %q carries a %s payload", c.Kind, o)
		}
		return ValidateID(c.Cancel.ID)

	case CommandPlan:
		if c.Plan == nil {
			return fmt.Errorf("protocol: command kind %q with no plan payload", c.Kind)
		}
		if o := other(c.Kind); o != "" {
			return fmt.Errorf("protocol: command kind %q carries a %s payload", c.Kind, o)
		}
		if err := ValidateID(c.Plan.ID); err != nil {
			return err
		}
		if c.Plan.Repo == "" {
			return fmt.Errorf("protocol: plan %s names no repo", c.Plan.ID)
		}
		return c.Plan.Event.validate()

	case "":
		return fmt.Errorf("protocol: command has no kind")

	default:
		return fmt.Errorf(
			"protocol: unknown command kind %q; the inbound set is closed to %q, %q and %q (INV-2)",
			c.Kind, CommandRun, CommandCancel, CommandPlan)
	}
}

// UnmarshalJSON decodes strictly and rejects anything it does not recognise.
//
// Two properties matter here, and both are INV-2 obligations rather than
// stylistic choices:
//
// Unknown kinds fail. Go's default decoding would leave an unrecognised kind
// as an ordinary string and both payloads nil, which a careless caller could
// read as "nothing to do" — a silent no-op where a refusal belongs.
//
// Unknown fields fail, at every level, including inside the payloads. That
// costs forward compatibility: a control plane that adds a field breaks older
// agents loudly. At a trust boundary that is the trade worth making, and the
// spec already treats adding a field as a security review. The alternative is
// an agent that silently ignores part of what it was sent.
func (c *Command) UnmarshalJSON(b []byte) error {
	// A distinct type breaks the recursion into this method.
	type wire Command

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	var w wire
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("protocol: decoding command: %w", err)
	}

	cmd := Command(w)
	if err := cmd.Validate(); err != nil {
		return err
	}
	if cmd.Cancel != nil {
		cmd.Cancel.Normalize()
	}

	*c = cmd
	return nil
}
