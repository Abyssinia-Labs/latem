package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/registry"
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
      - uses: actions/download-artifact@v4
        id: dl
        with: {name: app, path: restored}
      - run: ls ${{ steps.dl.outputs.download-path }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	a := job.Steps[0].Artifact
	if a == nil || !a.Download || a.Name != "app" || a.Path != "restored" {
		t.Fatalf("download = %+v", a)
	}
	if job.Steps[1].Env["GHA_EXPR_1"] != "restored" {
		t.Errorf("download-path = %+v", job.Steps[1].Env)
	}
}

func TestArtifactShimRefusals(t *testing.T) {
	cases := map[string]struct{ uses, with, want string }{
		"upload outside the workspace": {"upload-artifact@v4", "path: /tmp/report.xml", "outside the workspace"},
		"upload from runner.temp":      {"upload-artifact@v4", "path: ${{ runner.temp }}/x", "only known once the job is running"},
		"upload with no path":          {"upload-artifact@v4", "name: x", "requires the input"},
		"upload unknown input":         {"upload-artifact@v4", "path: dist\n          zip: true", "does not know"},
		"a bad retention":              {"upload-artifact@v4", "path: dist\n          retention-days: soon", "number of days"},
		"download from another run":    {"download-artifact@v4", "run-id: 42", "another run"},
		"download with a token":        {"download-artifact@v4", "github-token: x", "another run"},
		"download across a matrix":     {"download-artifact@v4", "pattern: build-*", "N2"},
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
