package workflow

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestUploadArtifactBecomesACollectingStep(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - run: make
      - uses: actions/upload-artifact@v4
        id: up
        if: always()
        with:
          name: build-${{ github.sha }}
          path: |
            ${{ github.workspace }}/dist
            reports/*.xml
            !dist/**/*.map
          retention-days: 5
          if-no-files-found: error
      - run: echo ${{ steps.up.outputs.artifact-id }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.Steps) != 3 {
		t.Fatalf("steps = %d", len(job.Steps))
	}
	up := job.Steps[1]
	a := up.Artifact
	if a == nil || a.Download {
		t.Fatalf("step = %+v", up)
	}
	if a.Name != "build-deadbeef" {
		t.Errorf("name = %q", a.Name)
	}
	// ${{ github.workspace }}/dist is the commonest spelling of a path in the
	// checkout, and becomes the relative path the collector reads.
	if strings.Join(a.Include, ",") != "dist,reports/*.xml" {
		t.Errorf("include = %q", a.Include)
	}
	// The explicit exclusion, then the hidden-file default.
	if a.Exclude[0] != "dist/**/*.map" || len(a.Exclude) != 1+len(hiddenFiles) {
		t.Errorf("exclude = %q", a.Exclude)
	}
	if a.Retain.Duration() != 5*24*time.Hour || a.IfNoFilesFound != "error" {
		t.Errorf("retain = %v, if-no-files-found = %q", a.Retain.Duration(), a.IfNoFilesFound)
	}
	// Its condition survives, because it is what decides whether it happens.
	if up.If == nil {
		t.Error("the upload's if: always() was dropped")
	}
	// artifact-id is known at admission and folded.
	if job.Steps[2].Env["GHA_EXPR_1"] != "build-deadbeef" {
		t.Errorf("artifact-id output = %+v", job.Steps[2].Env)
	}
	if err := RegistryFor("b", job).Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

// Since upload-artifact v4.4 hidden files are left out unless asked for, and
// §17's globs match a leading dot — so the default has to be written down.
func TestUploadIncludeHiddenFiles(t *testing.T) {
	job, err := compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/upload-artifact@v4\n        with: {path: dist, include-hidden-files: true}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Steps[0].Artifact.Exclude) != 0 {
		t.Errorf("include-hidden-files should drop the default exclusion: %q", job.Steps[0].Artifact.Exclude)
	}
	job, err = compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/upload-artifact@v4\n        with: {path: .}\n")
	if err != nil {
		t.Fatal(err)
	}
	a := job.Steps[0].Artifact
	if a.Name != "artifact" || a.Include[0] != "**" || len(a.Exclude) != len(hiddenFiles) {
		t.Errorf("path: . = %+v", a)
	}
}

func TestDownloadArtifact(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: actions/upload-artifact@v4
        with: {name: app, path: dist}
      - uses: actions/download-artifact@v4
        id: dl
        with: {name: app, path: restored}
      - run: ls ${{ steps.dl.outputs.download-path }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	a := job.Steps[1].Artifact
	if a == nil || !a.Download || a.Name != "app" || a.Path != "restored" {
		t.Fatalf("download = %+v", a)
	}
	if job.Steps[2].Env["GHA_EXPR_1"] != "restored" {
		t.Errorf("download-path = %+v", job.Steps[2].Env)
	}
}

// take-one's iPhone release (A77): the name is the version an earlier step
// computed, and the file is where xcodebuild exported it, in runner.temp.
// Neither is known at admission, so both are carried to the step.
func TestUploadNameAndPathFromStepOutputsAreLate(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - id: version
        run: echo "version=2026.11.7" >> "$GITHUB_OUTPUT"
      - uses: actions/upload-artifact@v7
        id: up
        with:
          name: takeone-ios-${{ steps.version.outputs.version }}-${{ github.run_number }}
          path: ${{ runner.temp }}/export/*.ipa
          if-no-files-found: error
      - run: echo "$ID"
        env:
          ID: ${{ steps.up.outputs.artifact-id }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	a := job.Steps[1].Artifact
	if a == nil || a.Late == nil || a.Name != "" || len(a.Include) != 0 {
		t.Fatalf("upload = %+v", a)
	}
	sc := registry.StepScope{
		Outputs: map[string]map[string]string{"version": {"version": "2026.11.7"}},
		TempDir: "/tmp/latem/temp",
	}
	if name, err := a.Late.Name.Resolve(sc); err != nil || name != "takeone-ios-2026.11.7-42" {
		t.Errorf("name resolves to %q, %v", name, err)
	}
	if len(a.Late.Include) != 1 {
		t.Fatalf("late include = %v", a.Late.Include)
	}
	if p, err := a.Late.Include[0].Resolve(sc); err != nil || p != "/tmp/latem/temp/export/*.ipa" {
		t.Errorf("path resolves to %q, %v", p, err)
	}
	// The hidden-file default still applies, wherever the files are.
	if len(a.Exclude) != len(hiddenFiles) {
		t.Errorf("exclude = %q", a.Exclude)
	}
	// artifact-id is the name, which is only known as the step runs: the
	// reference is left for the run loop to answer, not folded.
	if len(job.Steps[2].Late) != 1 {
		t.Errorf("artifact-id should be read as the step runs: env %v, late %v", job.Steps[2].Env, job.Steps[2].Late)
	}
	if err := RegistryFor("b", job).Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

// One late line makes every line late, so that the run loop can place them
// all and refuse a set that is in two places.
func TestUploadWithOneLatePathCarriesEveryPath(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - id: dmg
        run: echo "path=out/app.dmg" >> "$GITHUB_OUTPUT"
      - uses: actions/upload-artifact@v7
        with:
          path: |
            ${{ steps.dmg.outputs.path }}
            ${{ github.workspace }}/notes
            !notes/*.tmp
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	a := job.Steps[1].Artifact
	if a.Name != "artifact" || a.Late == nil || a.Late.Name != nil {
		t.Fatalf("upload = %+v", a)
	}
	var got []string
	for _, v := range append(append([]registry.LateValue(nil), a.Late.Include...), a.Late.Exclude...) {
		p, err := v.Resolve(registry.StepScope{Outputs: map[string]map[string]string{"dmg": {"path": "out/app.dmg"}}})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if strings.Join(got, ",") != "out/app.dmg,notes,notes/*.tmp" {
		t.Errorf("paths resolve to %q", got)
	}
	if len(a.Include) != 0 || len(a.Exclude) != len(hiddenFiles) {
		t.Errorf("static paths beside late ones: include %q, exclude %q", a.Include, a.Exclude)
	}
}

// upload-artifact v7's archive: false (A77) — take-one's extension release.
// One file, as it is, named after itself: GitHub ignores name, and so does
// this, and the outputs wait for the file.
func TestUploadArchiveFalse(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - id: version
        run: echo "version=1.2.3" >> "$GITHUB_OUTPUT"
      - uses: actions/upload-artifact@v7
        id: up
        with:
          name: ignored
          path: packages/extension/.output/takeone-extension-${{ steps.version.outputs.version }}-chrome.zip
          archive: false
          include-hidden-files: true
          if-no-files-found: error
      - run: echo ${{ steps.up.outputs.artifact-id }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	a := job.Steps[1].Artifact
	if !a.Single || a.Name != "" || a.Late == nil || a.Late.Name != nil || len(a.Late.Include) != 1 {
		t.Fatalf("upload = %+v", a)
	}
	if len(a.Exclude) != 0 {
		t.Errorf("include-hidden-files was dropped: %q", a.Exclude)
	}
	if err := RegistryFor("b", job).Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}

	// Known at admission, it is still named at the step, after the file.
	job, err = compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/upload-artifact@v7\n        with: {path: dist/app.zip, archive: false}\n")
	if err != nil {
		t.Fatal(err)
	}
	if a := job.Steps[0].Artifact; !a.Single || a.Name != "" || a.Late != nil || a.Include[0] != "dist/app.zip" {
		t.Errorf("static archive: false = %+v", a)
	}
	if err := RegistryFor("b", job).Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

// take-one's Mac release, within one job (A77): the DMG goes back to
// runner.temp, and download-path is where it went.
func TestDownloadIntoRunnerTemp(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: actions/upload-artifact@v7
        with: {name: dmg, path: out/app.dmg}
      - uses: actions/download-artifact@v8
        id: dl
        with:
          name: dmg
          path: ${{ runner.temp }}/dmg
      - run: ls "$DIR"
        env:
          DIR: ${{ steps.dl.outputs.download-path }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	a := job.Steps[1].Artifact
	if a.Name != "dmg" || a.Path != "" || a.Late == nil || a.Late.Path == nil {
		t.Fatalf("download = %+v", a)
	}
	if p, err := a.Late.Path.Resolve(registry.StepScope{TempDir: "/tmp/latem/temp"}); err != nil || p != "/tmp/latem/temp/dmg" {
		t.Errorf("path resolves to %q, %v", p, err)
	}
	if len(job.Steps[2].Late) != 1 {
		t.Errorf("download-path should be read as the step runs: %v", job.Steps[2].Env)
	}
	if err := RegistryFor("b", job).Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

// take-one's and wake-cloud's Mac publish jobs, and antescript's indexnow:
// each downloads what another job built. A run is one job, and its artifacts
// stay on the machine it ran on. With nothing saying which job's runs to read
// it is refused when the job is admitted — and `workflows check` says so —
// rather than failing at the step (A77). With the jobs the plan of the commit
// names, it compiles, and the step reads their runs (A84).
func TestDownloadOfAnotherJobsArtifact(t *testing.T) {
	wf := `
on:
  workflow_dispatch:
jobs:
  build:
    runs-on: [macos]
    outputs:
      version: ${{ steps.v.outputs.version }}
    steps:
      - id: v
        run: echo "version=1.0" >> "$GITHUB_OUTPUT"
      - uses: actions/upload-artifact@v7
        with:
          name: mac-${{ steps.v.outputs.version }}
          path: out/app.dmg
  publish:
    needs: build
    runs-on: [linux]
    steps:
      - uses: actions/download-artifact@v8
        with:
          name: mac-${{ needs.build.outputs.version }}
          path: ${{ runner.temp }}/dmg
`
	sc := runScope(map[string]protocol.NeedResult{"build": {Result: "success", Outputs: map[string]string{"version": "1.0"}}}, nil)
	_, err := compileRun(t, wf, "publish", sc)
	if err == nil || !strings.Contains(err.Error(), "no job it needs, uploads an artifact") {
		t.Fatalf("want the cross-job download refused by name, got %v", err)
	}
	if _, err := compileRun(t, wf, "build", sc); err != nil {
		t.Errorf("the uploading job is fine on its own: %v", err)
	}
	asked := 0
	job, err := CompileWith(t.Context(), parse(t, wf), "publish", sc, Options{
		WorkflowRun:   true,
		ArtifactsFrom: func() []string { asked++; return []string{"build"} },
	})
	if err != nil {
		t.Fatalf("with build named: %v", err)
	}
	if got := job.Steps[0].Artifact; got == nil || !slices.Equal(got.From, []string{"build"}) {
		t.Errorf("the download reads %+v, want build's runs", got)
	}
	if asked != 1 {
		t.Errorf("artifacts_from asked %d times", asked)
	}
	if err := RegistryFor("publish", job).Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
	// The uploading job is not asked: nothing in it downloads.
	if _, err := CompileWith(t.Context(), parse(t, wf), "build", sc, Options{
		WorkflowRun:   true,
		ArtifactsFrom: func() []string { t.Error("asked for a job with no download"); return nil },
	}); err != nil {
		t.Errorf("build: %v", err)
	}

	// A name nothing before it uploads, in a job that does upload something.
	_, err = compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/upload-artifact@v4\n        with: {name: logs, path: out}\n      - uses: actions/download-artifact@v4\n        with: {name: app}\n")
	if err == nil || !strings.Contains(err.Error(), `named "app"`) {
		t.Errorf("a name nothing uploaded: %v", err)
	}
	// A name only known as the upload runs could be any name.
	_, err = compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - id: v\n        run: echo v=1 >> $GITHUB_OUTPUT\n      - uses: actions/upload-artifact@v4\n        with: {name: 'app-${{ steps.v.outputs.v }}', path: out}\n      - uses: actions/download-artifact@v4\n        with: {name: app-1}\n")
	if err != nil {
		t.Errorf("a late upload name should leave the download to the step: %v", err)
	}
}

func TestArtifactShimRefusals(t *testing.T) {
	cases := map[string]struct{ uses, with, want string }{
		"upload outside the workspace": {"upload-artifact@v4", "path: /tmp/report.xml", "outside the workspace and runner.temp"},
		"upload with no path":          {"upload-artifact@v4", "name: x", "requires the input"},
		"upload unknown input":         {"upload-artifact@v4", "path: dist\n          zip: true", "does not know"},
		"a bad retention":              {"upload-artifact@v4", "path: dist\n          retention-days: soon", "number of days"},
		"archive not a boolean":        {"upload-artifact@v7", "path: dist\n          archive: zip", "not true or false"},
		"archive known only at a step": {"upload-artifact@v7", "path: dist\n          archive: ${{ steps.x.outputs.y }}", "decides what this step is"},
		"a secret in a name":           {"upload-artifact@v4", "path: dist\n          name: ${{ secrets.TOKEN }}", "reads a secret"},
		"a token in a path":            {"upload-artifact@v4", "path: out/${{ github.token }}", "reads a secret"},
		"download from another run":    {"download-artifact@v4", "run-id: 42", "another run"},
		"download with a token":        {"download-artifact@v4", "github-token: x", "another run"},
		"download across a matrix":     {"download-artifact@v4", "pattern: build-*", "N2"},
		"download from another job":    {"download-artifact@v4", "name: x", "no job it needs, uploads an artifact"},
		"the merge sub-action":         {"upload-artifact/merge@v4", "name: x", "not implemented"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/"+c.uses+"\n        with:\n          "+c.with+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// An upload of a name the registry cannot hold is refused at admission, not
// when the files are about to be copied.
func TestUploadNameIsValidatedAtAdmission(t *testing.T) {
	job, err := compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/upload-artifact@v4\n        with: {name: ../escape, path: dist}\n")
	if err != nil {
		t.Fatalf("compile should defer the name to validation: %v", err)
	}
	if err := (&registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"b": job}}).Validate(); err == nil {
		t.Error("a traversal name was admitted")
	}
}
