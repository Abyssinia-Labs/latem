package run

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const runWorkflowFile = `
name: Checks
on: [push]
jobs:
  web:
    needs: lint
    runs-on: [linux]
    outputs:
      version: ${{ steps.v.outputs.version }}
      leaked: ${{ steps.v.outputs.token }}
    steps:
      - id: v
        run: ./build.sh
        env:
          TAG: ${{ needs.lint.outputs.tag }}
          TOKEN: ${{ secrets.GITHUB_TOKEN }}
  web2:
    runs-on: [linux]
    steps: [{run: ./other.sh}]
  deploy:
    if: github.event_name == 'release'
    runs-on: [linux]
    steps: [{run: ./deploy.sh}]
  lint:
    runs-on: [linux]
    steps: [{run: make lint}]
`

const testToken = "ghs_0123456789abcdefABCDEF"

func workflowRequest(h *harness, job string) protocol.RunRequest {
	req := h.request(job)
	req.Ref = "refs/heads/main"
	req.Workflow = ".github/workflows/checks.yml"
	req.SHA = h.source.sha
	req.Event = &protocol.Event{Name: "push", Ref: "refs/heads/main", SHA: h.source.sha, Actor: "octocat",
		Payload: []byte(`{"head_commit":{"message":"; rm -rf /"}}`)}
	req.Needs = map[string]protocol.NeedResult{"lint": {Result: protocol.NeedSuccess, Outputs: map[string]string{"tag": "abc"}}}
	req.WorkflowRunID, req.RunNumber, req.RunAttempt = "wr7", 12, 1
	req.Token = testToken
	return req
}

// terminalUpdate is the run's terminal update, once the reporter has
// delivered it. Execute returns before it is delivered — reporting is
// asynchronous — so reading the last update straight away found `queued` on
// a busy CI machine and the terminal one only on a fast laptop.
func terminalUpdate(t *testing.T, h *harness, runID string) protocol.Update {
	t.Helper()
	var u protocol.Update
	waitFor(t, "the terminal update", func() bool {
		for _, r := range h.broker.updates() {
			if r.runID == runID && r.update.State.IsTerminal() {
				u = r.update
				return true
			}
		}
		return false
	})
	return u
}

// The token a job fetches with reaches gate 6 and no step (A72), and a repository
// that is not on GitHub never sends it.
func TestExecute_FetchToken(t *testing.T) {
	const fetchToken = "ghs_fetchFETCHfetch0123456789"
	run := func(t *testing.T, url string) (string, *harness, string) {
		t.Helper()
		h := newHarness(t)
		h.source.registry = ""
		h.cfg.Repos["app"].GitHub = "acme/app"
		h.cfg.Repos["app"].URL = url
		h.env.exec = func(_ context.Context, step executor.Step, stdout, _ io.Writer) (int, error) {
			for k, v := range step.Env {
				if strings.Contains(v, fetchToken) {
					t.Errorf("a step's %s holds the fetch token", k)
				}
			}
			_, _ = io.WriteString(stdout, "fetched with "+fetchToken+"\n")
			return 0, nil
		}
		o := h.build()
		req := workflowRequest(h, "web")
		req.FetchToken = fetchToken
		r := h.admitAndClaim(o, req)
		h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
		res, err := o.Execute(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		if res.State != protocol.StateSucceeded {
			t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
		}
		return h.source.fetched[0].Token, h, req.ID
	}

	t.Run("a GitHub repository fetches with it", func(t *testing.T) {
		got, h, id := run(t, "https://github.com/acme/app.git")
		if got != fetchToken {
			t.Errorf("gate 6 fetched with %q", got)
		}
		if strings.Contains(h.logContents(id), fetchToken) {
			t.Error("the fetch token reached the log")
		}
	})

	t.Run("a repository elsewhere does not", func(t *testing.T) {
		if got, _, _ := run(t, "https://git.example.com/acme/app.git"); got != "" {
			t.Errorf("gate 6 fetched with %q", got)
		}
	})
}

// A workflow run's job reads its needs and its event, gets its token as
// GITHUB_TOKEN and nowhere else, and reports its outputs as it ends (A52).
func TestExecute_WorkflowRunJob(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.cfg.Repos["app"].GitHub = "acme/app"
	h.env.outputs = func(executor.Step) map[string]string {
		return map[string]string{"version": "1.2.3", "token": testToken}
	}
	h.env.exec = func(_ context.Context, step executor.Step, stdout, _ io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "token is "+step.Env["TOKEN"]+"\n")
		return 0, nil
	}
	o := h.build()

	req := workflowRequest(h, "web")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}

	step := h.env.executed()[0]
	if step.Env["TAG"] != "abc" || step.Env["TOKEN"] != testToken {
		t.Errorf("TAG=%q TOKEN set=%v", step.Env["TAG"], step.Env["TOKEN"] == testToken)
	}
	spec := h.env.runSpec()
	for k, want := range map[string]string{
		"GITHUB_EVENT_NAME": "push", "GITHUB_ACTOR": "octocat", "GITHUB_RUN_ID": "wr7",
		"GITHUB_RUN_NUMBER": "12", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_JOB": "web", "GITHUB_WORKFLOW": "Checks",
	} {
		if spec.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, spec.Env[k], want)
		}
	}
	if _, ok := spec.Env["GITHUB_TOKEN"]; ok {
		t.Error("the token is in every step's environment; it is a secret a step names")
	}
	if !strings.Contains(string(spec.Event), `"; rm -rf /"`) {
		t.Errorf("event file %s", spec.Event)
	}

	u := terminalUpdate(t, h, req.ID)
	if u.State != protocol.StateSucceeded || u.Outputs["version"] != "1.2.3" {
		t.Errorf("terminal update %+v", u)
	}
	if _, ok := u.Outputs["leaked"]; ok {
		t.Error("an output holding the token was reported")
	}
	logs := h.logContents(req.ID)
	if strings.Contains(logs, testToken) {
		t.Error("the token reached the log")
	}
	if !strings.Contains(logs, "output leaked was dropped") {
		t.Errorf("the dropped output is not named in the log:\n%s", logs)
	}
}

// A job whose if: is false is skipped before a runner is chosen.
func TestExecute_WorkflowRunJobSkipped(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	o := h.build()
	req := workflowRequest(h, "deploy")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSkipped || len(h.env.executed()) != 0 {
		t.Fatalf("got %s with %d step(s), want skipped and none", res.State, len(h.env.executed()))
	}
	if u := terminalUpdate(t, h, req.ID); u.State != protocol.StateSkipped {
		t.Errorf("reported %s, want skipped", u.State)
	}
}

// The job builds the commit its event named, and only if that commit is on
// the ref (A52, INV-5).
func TestExecute_WorkflowRunJobPinsItsCommit(t *testing.T) {
	const older = "1111111111111111111111111111111111111111"
	const elsewhere = "2222222222222222222222222222222222222222"

	h := newHarness(t)
	h.source.registry = ""
	h.source.commits = map[string]bool{older: true, elsewhere: false}
	o := h.build()

	req := workflowRequest(h, "web2")
	req.SHA = older
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if co := h.source.checkedOut; len(co) == 0 || co[len(co)-1][1] != older {
		t.Errorf("checked out %v, want %s — the event's commit, not the ref's tip", co, older)
	}

	req = workflowRequest(h, "web2")
	req.ID, req.SHA = "run2", elsewhere
	res = h.run(o, req)
	if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrInvalidRef ||
		!strings.Contains(res.ErrorMsg, "is not on refs/heads/main") {
		t.Errorf("a commit off the ref: %s/%s %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
}

// A re-run of a pull request that has since merged: GitHub deleted its merge
// ref, and the job builds the commit its event named, fetched by its name when
// no ref here reaches it. A commit upstream no longer has is still refused, and
// a branch's commit is never fetched by its name.
func TestExecute_WorkflowRunJobAfterItsPullRequestMerged(t *testing.T) {
	const merge = "3333333333333333333333333333333333333333"
	const lost = "4444444444444444444444444444444444444444"

	for name, c := range map[string]struct {
		ref, sha  string
		inMirror  bool
		state     protocol.RunState
		wantInMsg string
		fetched   bool
	}{
		"in the mirror":       {"refs/pull/16/merge", merge, true, protocol.StateSucceeded, "", false},
		"fetched by its name": {"refs/pull/16/merge", merge, false, protocol.StateSucceeded, "", true},
		"gone upstream too":   {"refs/pull/16/merge", lost, false, protocol.StateRejected, "could not be fetched", true},
		"a deleted branch":    {"refs/heads/gone", merge, false, protocol.StateRejected, `ref "refs/heads/gone" does not exist`, false},
		"a branch's lost tip": {"refs/heads/main", lost, false, protocol.StateRejected, "is not in this machine's mirror", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.source.registry = ""
			h.source.upstream = map[string]bool{merge: true}
			h.cfg.Repos["app"].AllowedRefs = append(h.cfg.Repos["app"].AllowedRefs, "refs/pull/**")
			if c.inMirror {
				h.source.commits = map[string]bool{c.sha: false}
			}
			o := h.build()

			req := workflowRequest(h, "web2")
			req.Ref, req.SHA = c.ref, c.sha
			req.Event.Ref, req.Event.SHA = c.ref, c.sha
			r := h.admitAndClaim(o, req)
			h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", runWorkflowFile)
			res, err := o.Execute(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if res.State != c.state || !strings.Contains(res.ErrorMsg, c.wantInMsg) {
				t.Fatalf("got %s/%s %q, want %s containing %q", res.State, res.ErrorCode, res.ErrorMsg, c.state, c.wantInMsg)
			}
			if got := len(h.source.fetchedByName) > 0; got != c.fetched {
				t.Errorf("fetched by name: %v, want %v", h.source.fetchedByName, c.fetched)
			}
			if c.state == protocol.StateSucceeded {
				if co := h.source.checkedOut; len(co) == 0 || co[len(co)-1] != [2]string{c.ref, c.sha} {
					t.Errorf("checked out %v, want %s at %s", co, c.ref, c.sha)
				}
			}
		})
	}
}

const matrixWorkflowFile = `
on: [push]
jobs:
  test:
    strategy:
      matrix:
        os: [linux, macos]
    runs-on: ${{ matrix.os }}
    steps:
      - run: ./test.sh
        env:
          OS: ${{ matrix.os }}
          LEG: ${{ strategy.job-index }}/${{ strategy.job-total }}
`

// A55: a leg arrives as run.matrix, survives the row it is queued in, and is
// built only if the file at the pinned commit declares it — its runs-on read
// from the leg, so the macos leg finds no runner on a linux-only machine.
func TestExecute_MatrixLeg(t *testing.T) {
	for name, c := range map[string]struct {
		leg       string
		state     protocol.RunState
		code      protocol.ErrorCode
		wantInMsg string
	}{
		"the linux leg":        {`{"os":"linux"}`, protocol.StateSucceeded, "", ""},
		"the macos leg":        {`{"os":"macos"}`, protocol.StateRejected, protocol.ErrNoRunner, ""},
		"a leg the file lacks": {`{"os":"windows"}`, protocol.StateFailed, protocol.ErrBadRegistry, "is not one of the 2 legs"},
		"no leg for a matrix":  {``, protocol.StateFailed, protocol.ErrBadRegistry, "is a matrix of 2 jobs"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.source.registry = ""
			o := h.build()
			req := workflowRequest(h, "test")
			req.Needs = nil
			req.Matrix = json.RawMessage(c.leg)
			r := h.admitAndClaim(o, req)
			h.writeWorkspaceFile(req.ID, ".github/workflows/checks.yml", matrixWorkflowFile)
			res, err := o.Execute(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if res.State != c.state || res.ErrorCode != c.code || !strings.Contains(res.ErrorMsg, c.wantInMsg) {
				t.Fatalf("got %s/%s %q, want %s/%s containing %q", res.State, res.ErrorCode, res.ErrorMsg,
					c.state, c.code, c.wantInMsg)
			}
			if c.state != protocol.StateSucceeded {
				return
			}
			if env := h.env.executed()[0].Env; env["OS"] != "linux" || env["LEG"] != "0/2" {
				t.Errorf("OS=%q LEG=%q, want the leg's values and its place", env["OS"], env["LEG"])
			}
		})
	}
}
