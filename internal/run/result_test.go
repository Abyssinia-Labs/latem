package run

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/result"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A run leaves its result beside its log (A92): how each step ended, and what
// the failing step's output named as broken, with the secret it printed
// redacted there as in the log.
func TestExecute_ARunLeavesItsResult(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  check:
    runs_on: [linux]
    secrets: [MATCH_PASSWORD]
    inputs:
      channel:
        type: string
    steps:
      - name: build
        run: ./ci/build.sh
      - name: verify
        run: ./ci/verify.sh
`
	const secret = "correct horse battery staple"
	h.env.exec = func(_ context.Context, step executor.Step, stdout, stderr io.Writer) (int, error) {
		if step.Name != "build" {
			return 0, nil
		}
		_, _ = io.WriteString(stdout, "::warning file=ci/build.sh,line=2::the cache is cold\n")
		// The compiler names the file by its absolute path in the workspace,
		// as a tool given one does.
		_, _ = io.WriteString(stderr, "error[E0308]: mismatched types\n"+
			" --> "+h.env.runSpec().Workspace+"/src/lib.rs:3:18\n"+
			"  |\n"+
			"3 |     let x: i32 = \""+secret+"\";\n"+
			"  |                  ^^^ help: try `0`\n"+
			"\n"+
			"src/main.c:9:1: error: expected "+secret+"\n")
		return 101, nil
	}
	o := h.build()
	req := h.request("check")
	req.Inputs = map[string]string{"channel": "beta"}
	res, err := o.Execute(t.Context(), h.admitAndClaim(o, req))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateFailed {
		t.Fatalf("state = %s (%s), want failed", res.State, res.ErrorMsg)
	}

	r, err := result.Read(h.cfg.Paths.Logs, req.ID)
	if err != nil {
		t.Fatalf("reading the run's result: %v", err)
	}
	if !r.Settled || r.State != protocol.StateFailed || r.ErrorCode != string(protocol.ErrStep) ||
		r.ExitCode == nil || *r.ExitCode != 101 {
		t.Errorf("the ending: %+v", r)
	}
	if r.Job != "check" || r.Runner == "" || r.Workspace != "fresh" || r.SHA != h.source.sha ||
		r.Inputs["channel"] != "beta" || r.StartedAt == nil || r.FinishedAt == nil {
		t.Errorf("the description: %+v", r)
	}
	if len(r.Steps) < 2 || r.Steps[0].Name != "build" || r.Steps[0].Outcome != result.OutcomeFailure ||
		r.Steps[0].Errors != 2 || r.Steps[0].Warnings != 1 || r.Steps[1].Outcome != result.OutcomeSkipped {
		t.Fatalf("the steps: %+v", r.Steps)
	}
	if r.Errors != 2 || r.Warnings != 1 || len(r.Problems) != 3 {
		t.Fatalf("the problems: %+v", r.Problems)
	}
	rust := r.Problems[0]
	if rust.Tool != "rustc" || rust.File != "src/lib.rs" || rust.Line != 3 || rust.Column != 18 ||
		rust.Code != "E0308" || rust.Help != "try `0`" || rust.Step != "build" {
		t.Errorf("the compiler's error: %+v", rust)
	}
	if w := r.Problems[2]; w.Tool != "annotation" || w.Severity != result.SeverityWarning ||
		w.File != "ci/build.sh" || w.Line != 2 || w.Message != "the cache is cold" {
		t.Errorf("the step's own warning: %+v", w)
	}
	for _, p := range r.Problems {
		if strings.Contains(p.Message+p.Help, secret) {
			t.Errorf("a problem holds the secret the log redacts: %+v", p)
		}
	}
}

// A run that is refused before it has a step still says how it ended.
func TestExecute_ARejectedRunsResult(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	req := h.request("no-such-job")
	if _, err := o.Execute(t.Context(), h.admitAndClaim(o, req)); err != nil {
		t.Fatal(err)
	}
	r, err := result.Read(h.cfg.Paths.Logs, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Settled || r.State != protocol.StateRejected || r.ErrorCode == "" || len(r.Steps) != 0 || len(r.Problems) != 0 {
		t.Errorf("a rejected run's result: %+v", r)
	}
}
