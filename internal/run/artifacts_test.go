package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// artifactRun runs one workflow job on the harness and returns the result
// with every step the executor saw. tempHost says whether runner.temp is a
// directory the agent reaches, as native's is and docker's is not (A77).
// prepare runs before Execute, with the run's id and runner.temp's path.
func artifactRun(
	t *testing.T, tempHost bool, wf string,
	exec func(step executor.Step, temp string) int,
	outputs func(step executor.Step, temp string) map[string]string,
	prepare func(h *harness, runID, temp string),
) (Result, []executor.Step) {
	t.Helper()
	h := newHarness(t)
	h.source.registry = ""
	temp := t.TempDir()
	h.env.tempDir, h.env.hostTemp = temp, tempHost
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		if exec == nil {
			return 0, nil
		}
		return exec(step, temp), nil
	}
	if outputs != nil {
		h.env.outputs = func(step executor.Step) map[string]string { return outputs(step, temp) }
	}
	o := h.build()
	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", wf)
	if prepare != nil {
		prepare(h, req.ID, temp)
	}
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res, h.env.executed()
}

func artifactPaths(res Result) []string {
	var out []string
	for _, a := range res.Artifacts {
		out = append(out, a.Path)
	}
	slices.Sort(out)
	return out
}

func putFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// take-one's iPhone and Mac releases (A77): the artifact is named from a
// step's output, its file is in runner.temp — by `${{ runner.temp }}` and by
// a path a step printed — and it comes back to runner.temp within the job,
// where download-path says it is. The outputs are what happened.
func TestExecute_ArtifactNamedFromOutputsAndKeptInRunnerTemp(t *testing.T) {
	var restored string
	res, steps := artifactRun(t, true, `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - id: version
        name: version
        run: ./version.sh
      - id: dmg
        name: dmg
        run: ./dmg.sh
      - uses: actions/upload-artifact@v7
        id: ipa
        with:
          name: takeone-ios-${{ steps.version.outputs.version }}
          path: ${{ runner.temp }}/export/*.ipa
          if-no-files-found: error
      - uses: actions/upload-artifact@v7
        id: mac
        with:
          name: takeone-mac-${{ steps.version.outputs.version }}
          path: ${{ steps.dmg.outputs.path }}
          if-no-files-found: error
      - uses: actions/download-artifact@v8
        id: dl
        with:
          name: takeone-mac-${{ steps.version.outputs.version }}
          path: ${{ runner.temp }}/dmg
      - name: check
        run: ./check.sh
        env:
          IPA: ${{ steps.ipa.outputs.artifact-id }}
          MAC: ${{ steps.mac.outputs.artifact-id }}
          DIR: ${{ steps.dl.outputs.download-path }}
`, func(step executor.Step, temp string) int {
		switch step.Name {
		case "dmg":
			putFile(t, filepath.Join(temp, "export", "TakeOne.ipa"), "ipa")
			putFile(t, filepath.Join(temp, "export", "notes.txt"), "not this")
			putFile(t, filepath.Join(temp, "TakeOne-2026.11.7.dmg"), "dmg")
		case "check":
			if b, err := os.ReadFile(filepath.Join(step.Env["DIR"], "TakeOne-2026.11.7.dmg")); err == nil {
				restored = string(b)
			}
		}
		return 0
	}, func(step executor.Step, temp string) map[string]string {
		switch step.Name {
		case "version":
			return map[string]string{"version": "2026.11.7"}
		case "dmg":
			return map[string]string{"path": filepath.Join(temp, "TakeOne-2026.11.7.dmg")}
		}
		return nil
	}, nil)

	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	// Rooted as upload-artifact roots them: the pattern's directory, and a
	// lone file's own.
	want := []string{"takeone-ios-2026.11.7/TakeOne.ipa", "takeone-mac-2026.11.7/TakeOne-2026.11.7.dmg"}
	if got := artifactPaths(res); !slices.Equal(got, want) {
		t.Errorf("artifacts = %v, want %v", got, want)
	}
	check := steps[len(steps)-1]
	if check.Env["IPA"] != "takeone-ios-2026.11.7" || check.Env["MAC"] != "takeone-mac-2026.11.7" {
		t.Errorf("artifact-id outputs = %q, %q", check.Env["IPA"], check.Env["MAC"])
	}
	if !strings.HasSuffix(check.Env["DIR"], "/dmg") || !filepath.IsAbs(check.Env["DIR"]) {
		t.Errorf("download-path = %q, want runner.temp's dmg", check.Env["DIR"])
	}
	if restored != "dmg" {
		t.Errorf("the DMG restored into runner.temp reads %q", restored)
	}
}

// upload-artifact v7's archive: false (A77): one file, as it is, named after
// itself whatever name: says — take-one's extension zip, which WXT writes to
// a dot-directory. Two files are refused, as GitHub refuses them.
func TestExecute_ArchiveFalseUploadsOneFileNamedAfterIt(t *testing.T) {
	wf := `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - id: zip
        name: zip
        run: ./zip.sh
      - uses: actions/upload-artifact@v7
        id: up
        with:
          name: ignored
          path: packages/extension/${{ steps.zip.outputs.zip }}
          archive: false
          include-hidden-files: true
          if-no-files-found: error
      - name: check
        run: ./check.sh
        env:
          ID: ${{ steps.up.outputs.artifact-id }}
`
	zip := func(name string) func(executor.Step, string) map[string]string {
		return func(step executor.Step, _ string) map[string]string {
			if step.Name == "zip" {
				return map[string]string{"zip": name}
			}
			return nil
		}
	}
	files := func(h *harness, runID, _ string) {
		h.writeWorkspaceFile(runID, "packages/extension/.output/takeone-extension-1.2.3.zip", "zip")
		h.writeWorkspaceFile(runID, "packages/extension/.output/takeone-extension-1.2.3.crx", "crx")
	}

	res, steps := artifactRun(t, true, wf, nil, zip(".output/takeone-extension-1.2.3.zip"), files)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if got := artifactPaths(res); !slices.Equal(got, []string{"takeone-extension-1.2.3.zip/takeone-extension-1.2.3.zip"}) {
		t.Errorf("artifacts = %v", got)
	}
	if id := steps[len(steps)-1].Env["ID"]; id != "takeone-extension-1.2.3.zip" {
		t.Errorf("artifact-id = %q", id)
	}

	res, _ = artifactRun(t, true, wf, nil, zip(".output/*"), files)
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrArtifact ||
		!strings.Contains(res.ErrorMsg, "one file") {
		t.Errorf("two files under archive: false = %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
}

// What an artifact step cannot reach is refused at the step, naming why: a
// tart runner's runner.temp is inside its VM, which neither the agent nor a
// copy reaches (docker's is copied, A84), and anywhere but the workspace and
// runner.temp is not an artifact step's to read (A77).
func TestExecute_ArtifactPathsOutOfReach(t *testing.T) {
	cases := map[string]struct {
		hostTemp bool
		path     func(temp string) string
		want     string
	}{
		"a VM's runner.temp": {false, func(temp string) string { return filepath.Join(temp, "out.txt") }, "inside its VM"},
		"outside both":       {true, func(string) string { return "/etc/hosts" }, "outside the workspace and runner.temp"},
		"a home directory":   {true, func(string) string { return "~/.ssh/id_ed25519" }, "home directory"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			res, _ := artifactRun(t, c.hostTemp, `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - id: out
        name: out
        run: ./out.sh
      - uses: actions/upload-artifact@v7
        with:
          name: out
          path: ${{ steps.out.outputs.path }}
`, func(step executor.Step, temp string) int {
				putFile(t, filepath.Join(temp, "out.txt"), "x")
				return 0
			}, func(step executor.Step, temp string) map[string]string {
				return map[string]string{"path": c.path(temp)}
			}, nil)
			if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrArtifact ||
				!strings.Contains(res.ErrorMsg, c.want) {
				t.Errorf("got %s/%s: %s; want E_ARTIFACT naming %q", res.State, res.ErrorCode, res.ErrorMsg, c.want)
			}
			if len(res.Artifacts) != 0 {
				t.Errorf("collected %v", artifactPaths(res))
			}
		})
	}
}
