package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A release, cut down: publish downloads what build uploaded, as the Mac
// releases and sokie's CLI do; other uploads the same name and is no job
// publish needs.
const releaseWorkflow = `
name: Release
on: [push]
jobs:
  build:
    runs-on: [linux]
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: dist, path: dist}
  other:
    runs-on: [linux]
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: dist, path: other}
  publish:
    needs: build
    runs-on: [linux]
    steps:
      - uses: actions/download-artifact@v8
        with: {name: dist, path: out}
      - name: check
        run: ./check.sh
`

// releaseJob runs one job of releaseWorkflow as a workflow run's job, in the
// workflow run wr, with files written into its workspace first.
func releaseJob(t *testing.T, h *harness, o *Orchestrator, id, job, wr string, files map[string]string) Result {
	t.Helper()
	req := workflowRequest(h, job)
	req.ID, req.Workflow, req.WorkflowRunID = id, ".github/workflows/release.yml", wr
	req.Needs = nil
	if job == "publish" {
		req.Needs = map[string]protocol.NeedResult{"build": {Result: protocol.NeedSuccess}}
	}
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(id, ".github/workflows/release.yml", releaseWorkflow)
	for rel, body := range files {
		h.writeWorkspaceFile(id, rel, body)
	}
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute %s: %v", id, err)
	}
	return res
}

// A download reads what a job it needs uploaded, from that job's run on this
// machine in the same workflow run (A84) — not from a job it does not need,
// though it uploaded the same name, and not from another workflow run.
func TestExecute_ADownloadReadsWhatAJobItNeedsUploaded(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	var got string
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		if step.Name == "check" {
			b, _ := os.ReadFile(filepath.Join(h.env.spec.Workspace, "out", "app.txt"))
			got = string(b)
		}
		return 0, nil
	}
	o := h.build()

	if res := releaseJob(t, h, o, "r-build", "build", "wr7", map[string]string{"dist/app.txt": "build's"}); res.State != protocol.StateSucceeded {
		t.Fatalf("build: %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if res := releaseJob(t, h, o, "r-other", "other", "wr7", map[string]string{"other/app.txt": "other's"}); res.State != protocol.StateSucceeded {
		t.Fatalf("other: %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	res := releaseJob(t, h, o, "r-publish", "publish", "wr7", nil)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("publish: %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if got != "build's" {
		t.Errorf("publish read %q, want what build uploaded", got)
	}
	if log := h.logContents("r-publish"); !strings.Contains(log, "uploaded by build") {
		t.Errorf("the log does not say whose artifact it was:\n%s", log)
	}

	// The same job in another workflow run, where build never ran here.
	res = releaseJob(t, h, o, "r-publish-2", "publish", "wr8", nil)
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrArtifact ||
		!strings.Contains(res.ErrorMsg, "no run of build in this workflow run finished on this machine") {
		t.Errorf("another workflow run: %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
}

// On docker, runner.temp is in the container: what an upload reads there is
// copied out first, and what a download writes there is copied in (A84) —
// sokie's pack uploads from $RUNNER_TEMP/pack, and the Mac releases' publish
// downloads into $RUNNER_TEMP/dmg.
func TestExecute_ArtifactsInADockerRunnersTemp(t *testing.T) {
	const wf = `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - id: pack
        name: pack
        run: ./pack.sh
      - uses: actions/upload-artifact@v7
        with:
          name: cli-package
          path: ${{ runner.temp }}/pack/${{ steps.pack.outputs.tarball }}
          if-no-files-found: error
      - uses: actions/download-artifact@v8
        id: dl
        with:
          name: cli-package
          path: ${{ runner.temp }}/back
      - name: check
        run: ./check.sh
        env:
          DIR: ${{ steps.dl.outputs.download-path }}
`
	h := newHarness(t)
	h.source.registry = ""
	temp := t.TempDir()
	h.env.tempDir, h.env.copies = temp, true
	var back string
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		switch step.Name {
		case "pack":
			putFile(t, filepath.Join(temp, "pack", "sokie-1.0.0.tgz"), "tgz")
			putFile(t, filepath.Join(temp, "global", "lib", "huge.js"), "not this")
		case "check":
			b, _ := os.ReadFile(filepath.Join(temp, "back", "sokie-1.0.0.tgz"))
			back = string(b)
			if step.Env["DIR"] != filepath.Join(temp, "back") {
				t.Errorf("download-path = %q", step.Env["DIR"])
			}
		}
		return 0, nil
	}
	h.env.outputs = func(step executor.Step) map[string]string {
		if step.Name == "pack" {
			return map[string]string{"tarball": "sokie-1.0.0.tgz"}
		}
		return nil
	}
	o := h.build()
	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", wf)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if got := artifactPaths(res); len(got) != 1 || got[0] != "cli-package/sokie-1.0.0.tgz" {
		t.Errorf("artifacts = %v", got)
	}
	if back != "tgz" {
		t.Errorf("the tarball copied back into runner.temp reads %q", back)
	}
	// Nothing is left beside the workspace once the steps are done.
	if left, _ := filepath.Glob(filepath.Join(h.source.root, req.ID+".artifact-*")); len(left) != 0 {
		t.Errorf("staging left behind: %v", left)
	}
}

func TestLiteralPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"pack/sokie-1.0.0.tgz": "pack/sokie-1.0.0.tgz",
		"pack/*.tgz":           "pack",
		"export/**/*.ipa":      "export",
		"**":                   "",
		"./dmg":                "dmg",
		"a/b?/c":               "a",
	} {
		if got := literalPrefix(in); got != want {
			t.Errorf("literalPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
