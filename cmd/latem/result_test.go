package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/result"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const resultRun = "r_0123456789abcdef01234567"

// writeResult builds a result as the agent does, through the collector, and
// leaves it beside where the run's log would be: a lint step that passed with
// a warning, and a test step that failed with a compiler error.
func writeResult(t *testing.T, dir, runID string, finish bool) {
	t.Helper()
	c := result.NewCollector(filepath.Join(dir, "logs"), result.Header{RunID: runID, Repo: "app", Job: "test", Ref: "main"})
	c.Describe("refs/heads/main", strings.Repeat("a", 40), "test", "warm", time.Now().Add(-10*time.Second))
	line := func(s string) []protocol.LogFrame {
		return []protocol.LogFrame{{RunID: runID, Stream: protocol.StreamStderr, Line: s}}
	}
	c.StepStart("lint")
	_ = c.Write(line("src/a.c:3:1: warning: unused variable 'a'"))
	c.StepEnd(result.OutcomeSuccess, false, nil, "")
	c.StepStart("unit tests")
	for _, l := range []string{"error[E0308]: mismatched types", " --> src/lib.rs:3:18", "  |",
		"3 |     let x: i32 = \"a\";", "  |                  ^^^ help: try `0`", ""} {
		_ = c.Write(line(l))
	}
	if !finish {
		return
	}
	code := 101
	c.StepEnd(result.OutcomeFailure, false, &code, "")
	if err := c.Finish(protocol.StateFailed, protocol.ErrStep, `step "unit tests" exited 101`, &code); err != nil {
		t.Fatal(err)
	}
}

func seedFinishedRun(t *testing.T, db *store.DB, id string, state protocol.RunState) {
	t.Helper()
	started, finished := time.Now().Add(-10*time.Second), time.Now()
	r := store.Run{ID: id, Repo: "app", Job: "test", Ref: "main", State: state, OriginKind: "cli",
		ErrorMsg: `step "unit tests" exited 101`, StartedAt: &started, FinishedAt: &finished}
	if state == protocol.StateFailed {
		r.ErrorCode = protocol.ErrStep
	}
	if err := db.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// `latem result` says how the run went and what broke, where, for a person;
// --json is the same result for a program.
func TestResultCommand(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedFinishedRun(t, db, resultRun, protocol.StateFailed)
	writeResult(t, dir, resultRun, true)

	stdout, _, err := execCommand(t, "result", resultRun)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	for _, want := range []string{
		resultRun + " failed (E_STEP)", "job test of app at aaaaaaaaaaaa", "warm workspace",
		"lint", "unit tests", "failure, exit 101",
		"1 error(s), 1 warning(s)",
		"error[E0308] src/lib.rs:3:18", "mismatched types", "help: try `0`",
		"warning src/a.c:3:1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the result does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Index(stdout, "error[E0308]") > strings.Index(stdout, "warning src/a.c") {
		t.Errorf("the warning is printed before the error:\n%s", stdout)
	}

	stdout, _, err = execCommand(t, "result", resultRun, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res result.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, stdout)
	}
	if !res.Settled || res.State != protocol.StateFailed || len(res.Steps) != 2 || len(res.Problems) != 2 ||
		res.Problems[0].File != "src/lib.rs" || res.Problems[0].Help != "try `0`" {
		t.Errorf("--json: %+v", res)
	}

	if _, _, err := execCommand(t, "result", "r_ffffffffffffffffffffffff"); err == nil || exitCode(err) != ExitFailure {
		t.Errorf("an unknown run: %v", err)
	}
}

// A run with no stored result still answers, from its row, and says that is
// all there is. One its agent did not see end takes its ending from the row.
func TestResultCommandWithoutAStoredResult(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedFinishedRun(t, db, resultRun, protocol.StateFailed)
	stdout, _, err := execCommand(t, "result", resultRun, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res result.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Settled || res.State != protocol.StateFailed || res.ErrorCode != "E_STEP" ||
		!strings.Contains(res.Note, "no result is stored") || res.Steps == nil || res.Problems == nil {
		t.Errorf("a run with no stored result: %+v", res)
	}

	const killed = "r_1111111111111111111111aa"
	seedFinishedRun(t, db, killed, protocol.StateInterrupted)
	writeResult(t, dir, killed, false)
	// The agent was killed under the second step: one step is on disk.
	c := result.NewCollector(filepath.Join(dir, "logs"), result.Header{RunID: killed, Repo: "app", Job: "test"})
	c.StepStart("lint")
	c.StepEnd(result.OutcomeSuccess, false, nil, "")
	stdout, _, err = execCommand(t, "result", killed, "--json")
	if err != nil {
		t.Fatal(err)
	}
	res = result.Result{}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Settled || res.State != protocol.StateInterrupted || len(res.Steps) != 1 ||
		!strings.Contains(res.Note, "as far as it got") {
		t.Errorf("an interrupted run's result: %+v", res)
	}
}

// waitForRun and getRun answer with the result, so an agent that started a
// run reads what broke without reading the log.
func TestMCPRunsCarryTheirResult(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedFinishedRun(t, db, resultRun, protocol.StateFailed)
	writeResult(t, dir, resultRun, true)
	seedCLIRun(t, db, "r_queued", protocol.StateQueued)
	c := startMCP(t)

	for _, tool := range []string{"waitForRun", "getRun"} {
		v, msg := c.tool(tool, map[string]any{"runId": resultRun})
		if msg != "" {
			t.Fatalf("%s: %s", tool, msg)
		}
		run := v
		if tool == "waitForRun" {
			if v["settled"] != true {
				t.Fatalf("waitForRun = %v", v)
			}
			run = v["run"].(map[string]any)
		}
		res, ok := run["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s carries no result: %v", tool, run)
		}
		steps, problems := res["steps"].([]any), res["problems"].([]any)
		first := problems[0].(map[string]any)
		if len(steps) != 2 || len(problems) != 2 || res["errors"] != float64(1) || res["workspace"] != "warm" ||
			first["file"] != "src/lib.rs" || first["line"] != float64(3) || first["code"] != "E0308" ||
			first["help"] != "try `0`" || first["step"] != "unit tests" {
			t.Errorf("%s's result: %v", tool, res)
		}
	}
	listed, _ := c.tool("listRuns", map[string]any{})
	for _, r := range runsOf(t, listed) {
		if r["result"] != nil {
			t.Errorf("a listing carries a result: %v", r)
		}
	}
	// A run that has not started has nothing to say beyond itself.
	if v, _ := c.tool("getRun", map[string]any{"runId": "r_queued"}); v["result"] != nil {
		t.Errorf("a queued run carries a result: %v", v["result"])
	}
}

// A client that sends a progress token is told each state and each step as
// it ends while it waits, and then gets the answer as the call's result. One
// that sends none is sent nothing but the answer.
func TestMCPWaitForRunSaysWhatHappensWhileItWaits(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	const id = "r_2222222222222222222222bb"
	seedCLIRun(t, db, id, protocol.StateRunning)
	c := startMCP(t)

	c.send(`{"jsonrpc":"2.0","id":"w","method":"tools/call","params":{"name":"waitForRun","arguments":{"runId":"` +
		id + `","timeoutSeconds":60},"_meta":{"progressToken":"tok-1"}}}`)
	progress := func() string {
		t.Helper()
		m := c.read()
		if m["method"] != "notifications/progress" {
			t.Fatalf("want a progress notification, got %v", m)
		}
		p := m["params"].(map[string]any)
		if p["progressToken"] != "tok-1" {
			t.Fatalf("the notification is for %v", p["progressToken"])
		}
		return p["message"].(string)
	}
	if got := progress(); got != "running" {
		t.Errorf("the first thing told is %q, want the run's state", got)
	}

	// A step ends: the agent writes the result, and the waiter says so.
	col := result.NewCollector(filepath.Join(dir, "logs"), result.Header{RunID: id, Repo: "app", Job: "test"})
	col.StepStart("build")
	_ = col.Write([]protocol.LogFrame{{RunID: id, Stream: protocol.StreamStderr, Line: "src/a.c:1:1: error: no"}})
	one := 1
	col.StepEnd(result.OutcomeFailure, false, &one, "")
	if got := progress(); !strings.HasPrefix(got, "step build: failure in ") || !strings.HasSuffix(got, ", 1 error(s)") {
		t.Errorf("the step's ending is told as %q", got)
	}

	if err := db.Transition(context.Background(), id, protocol.StateCollecting, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	if got := progress(); got != "collecting" {
		t.Errorf("told %q, want collecting", got)
	}
	_ = col.Finish(protocol.StateFailed, protocol.ErrStep, "step build exited 1", &one)
	if err := db.Transition(context.Background(), id, protocol.StateFailed, protocol.Update{ErrorCode: protocol.ErrStep}); err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	for answer == nil {
		m := c.read()
		if m["method"] == "notifications/progress" {
			continue // "failed", told on the way out
		}
		answer = m
	}
	if answer["id"] != "w" || answer["result"] == nil {
		t.Fatalf("the answer: %v", answer)
	}
	text := answer["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"settled":true`) || !strings.Contains(text, `"file":"src/a.c"`) {
		t.Errorf("the answer does not carry the settled run and its result: %s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", id+result.Suffix)); err != nil {
		t.Fatal(err)
	}
}
