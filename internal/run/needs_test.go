package run

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/system"
)

// A run's log says what its job needs that the runner lacks, before any step
// runs, and runs exactly as it would have (A59).
func TestExecute_NotesWhatTheRunnerLacks(t *testing.T) {
	h := newHarness(t)
	h.source = newFakeSource(t, `version: 1
jobs:
  ios:
    runs_on: [macos]
    steps:
      - name: build
        run: |
          xcodegen generate
          xcodebuild -scheme App -destination 'platform=iOS Simulator,id=SIM' test
`)
	h.exec.name = "native"
	h.runners = []Runner{{Name: "mac", Labels: []string{"macos"}, Executor: h.exec}}
	h.opts.Toolchains = func() *system.Toolchains {
		return &system.Toolchains{
			Xcodes: []system.XcodeInstall{{Version: system.Version{Major: 26, Text: "26.0.1"}}},
			Tools:  map[string]bool{},
		}
	}
	o := h.build()
	req := h.request("ios")
	req.Repo = "strict"
	res := h.run(o, req)
	log := h.logContents(res.RunID)
	for _, want := range []string{"this job needs xcodegen", "this job needs an iOS simulator runtime"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// With no probe, no note.
func TestExecute_NoToolchainsNoNote(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	req := h.request("macos")
	req.Repo = "strict"
	res := h.run(o, req)
	if log := h.logContents(res.RunID); strings.Contains(log, "this job needs") {
		t.Errorf("a note without a probe:\n%s", log)
	}
}
