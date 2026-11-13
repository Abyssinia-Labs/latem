package result

import (
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Collector builds one run's [Result] as the run goes.
//
// It is a sink of the run's log pipeline (logstream.Sink), so every line it
// reads has been through the redactor and is exactly what the log holds. The
// run tells it where the steps begin and end; what a step printed between
// those is read for the problems it names.
//
// The file is written when a step ends and when the run does. A reader mid-run
// sees the steps so far.
type Collector struct {
	mu     sync.Mutex
	logDir string
	now    func() time.Time
	res    Result
	roots  []string

	step    *Step
	started time.Time
	scans   map[protocol.Stream]*scanner
	// seen is every problem kept or counted, so that one a tool prints twice
	// (cargo prints a warning for the library and again for its tests) is
	// one.
	seen map[Problem]bool
	// direct are the problems the current step named itself, with a
	// workflow command.
	direct []Problem

	keptErrors, keptWarnings int
}

// Header is what a run is, known before its first step.
type Header struct {
	RunID, Repo, Job, Ref, SHA, Base string
	Inputs                           map[string]string
}

// NewCollector starts a result for a run whose log is in logDir.
func NewCollector(logDir string, h Header) *Collector {
	return &Collector{
		logDir: logDir,
		now:    time.Now,
		res: Result{
			Version: Version, RunID: h.RunID, Repo: h.Repo, Job: h.Job, Ref: h.Ref, SHA: h.SHA, Base: h.Base,
			Inputs: h.Inputs, Steps: []Step{}, Problems: []Problem{},
		},
		seen: map[Problem]bool{},
	}
}

// Describe adds what gates 9 to 13 settled: the commit, the runner, the kind
// of workspace, and the directories a tool may put in front of a path.
func (c *Collector) Describe(ref, sha, runner, workspace string, started time.Time, roots ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ref != "" {
		c.res.Ref = ref
	}
	if sha != "" {
		c.res.SHA = sha
	}
	if runner != "" {
		c.res.Runner = runner
	}
	if workspace != "" {
		c.res.Workspace = workspace
	}
	if !started.IsZero() {
		c.res.StartedAt = &started
	}
	if len(roots) > 0 {
		c.roots = roots
	}
}

// Write reads log frames for the problems they name. Only a step's own
// output is read: the agent's lines are latem's, and say nothing a field of
// the result does not.
func (c *Collector) Write(frames []protocol.LogFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.step == nil {
		return nil
	}
	for _, f := range frames {
		if f.Stream != protocol.StreamStdout && f.Stream != protocol.StreamStderr {
			continue
		}
		sc := c.scans[f.Stream]
		if sc == nil {
			sc = &scanner{roots: c.roots}
			c.scans[f.Stream] = sc
		}
		sc.line(f.Line)
	}
	return nil
}

// Close is the sink's. The result is written by [Collector.Finish], which
// knows how the run ended; a pipeline closing does not.
func (c *Collector) Close() error { return nil }

// Annotate takes a problem a step named itself, with a `::error` or
// `::warning` workflow command. The text and parameters are as the log shows
// them: redacted.
func (c *Collector) Annotate(level, message string, params map[string]string) {
	if c == nil {
		return
	}
	if level != SeverityError && level != SeverityWarning {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.step == nil {
		return
	}
	p := Problem{Severity: level, Message: message, File: params["file"], Code: params["title"], Tool: "annotation"}
	p.Line, p.Column = atoi(params["line"]), atoi(params["col"])
	c.direct = append(c.direct, p)
}

// StepStart begins a step: what is printed from here is its.
func (c *Collector) StepStart(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = &Step{Name: name, Outcome: OutcomeRunning}
	c.started = c.now()
	c.scans = map[protocol.Stream]*scanner{}
	c.direct = nil
}

// StepSkipped records a step that did not run.
func (c *Collector) StepSkipped(name, reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res.Steps = append(c.res.Steps, Step{Name: name, Outcome: OutcomeSkipped, Reason: reason})
}

// StepEnd ends the step StepStart began and keeps its problems.
//
// A step that succeeded keeps its warnings and loses the errors the readers
// found in it. They recognise shapes, and a step that passed while printing
// the shape of an error was showing one, not having one: a compile-fail
// test's expected diagnostics, a tool listing what it fixed. An error the
// step named itself with `::error` is its own statement and stays.
func (c *Collector) StepEnd(outcome string, forgiven bool, exitCode *int, reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	step := c.step
	if step == nil {
		c.mu.Unlock()
		return
	}
	step.Outcome, step.Forgiven, step.ExitCode, step.Reason = outcome, forgiven, exitCode, reason
	step.DurationMs = c.now().Sub(c.started).Milliseconds()

	found := append([]Problem(nil), c.direct...)
	for _, stream := range []protocol.Stream{protocol.StreamStderr, protocol.StreamStdout} {
		if sc := c.scans[stream]; sc != nil {
			sc.flush()
			for _, p := range sc.out {
				if outcome == OutcomeSuccess && p.Severity == SeverityError {
					continue
				}
				found = append(found, p)
			}
		}
	}
	for _, p := range found {
		p.Step = step.Name
		p.Message = cut(p.Message, MaxMessageBytes)
		p.Help = cut(p.Help, MaxHelpBytes)
		if c.seen[p] {
			continue
		}
		c.seen[p] = true
		c.keep(step, p)
	}
	c.res.Steps = append(c.res.Steps, *step)
	c.step, c.scans, c.direct = nil, nil, nil
	c.mu.Unlock()
	_ = c.flush()
}

func (c *Collector) keep(step *Step, p Problem) {
	if p.Severity == SeverityError {
		step.Errors++
		c.res.Errors++
		if c.keptErrors == MaxErrors {
			c.res.Dropped++
			return
		}
		c.keptErrors++
	} else {
		step.Warnings++
		c.res.Warnings++
		if c.keptWarnings == MaxWarnings {
			c.res.Dropped++
			return
		}
		c.keptWarnings++
	}
	c.res.Problems = append(c.res.Problems, p)
}

// Finish records how the run ended and writes the result a last time. A step
// still open, because the run was stopped under it, ends as the run did.
func (c *Collector) Finish(state protocol.RunState, code protocol.ErrorCode, msg string, exitCode *int) error {
	c.mu.Lock()
	open := c.step != nil
	c.mu.Unlock()
	if open {
		outcome := OutcomeFailure
		switch state {
		case protocol.StateCancelled:
			outcome = OutcomeCancelled
		case protocol.StateTimedOut:
			outcome = OutcomeTimedOut
		}
		c.StepEnd(outcome, false, exitCode, "")
	}
	c.mu.Lock()
	now := c.now()
	c.res.State, c.res.Settled = state, state.IsTerminal()
	c.res.ErrorCode, c.res.Error, c.res.ExitCode = string(code), msg, exitCode
	c.res.FinishedAt = &now
	if c.res.StartedAt != nil {
		c.res.DurationMs = now.Sub(*c.res.StartedAt).Milliseconds()
	}
	c.mu.Unlock()
	return c.flush()
}

// flush writes the result as it stands.
func (c *Collector) flush() error {
	snapshot := c.Result()
	return write(c.logDir, &snapshot)
}

// Result is the result as it stands. Errors first, each kind in the order it
// was printed: what broke the build before what the build remarked on. Its
// lists are never nil, so a run with no problems says `[]` and a caller that
// loops over them need not ask first.
func (c *Collector) Result() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.res
	out.Steps = append(make([]Step, 0, len(c.res.Steps)), c.res.Steps...)
	out.Problems = append(make([]Problem, 0, len(c.res.Problems)), c.res.Problems...)
	sort.SliceStable(out.Problems, func(i, j int) bool {
		return out.Problems[i].Severity == SeverityError && out.Problems[j].Severity != SeverityError
	})
	return out
}

// cut shortens s to at most n bytes, on a rune boundary, and says it did.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + " …"
}
