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

// An App Store update that lands while a job runs on a native runner is said
// in its log as the job ends (A64); a job whose Xcode held still says nothing.
func TestExecute_NotesAnXcodeChangedMidJob(t *testing.T) {
	for name, after := range map[string]string{"updated": "27.3", "unchanged": "27.2"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.exec.name = "native"
			h.runners = []Runner{{Name: "mac", Labels: []string{"macos", "xcode"}, Executor: h.exec}}
			calls := 0
			h.opts.Toolchains = func() *system.Toolchains {
				calls++
				v := "27.2"
				if calls > 1 {
					v = after
				}
				return &system.Toolchains{Xcodes: []system.XcodeInstall{{Version: system.Version{Major: 27, Text: v}}}}
			}
			o := h.build()
			req := h.request("macos")
			req.Repo = "strict"
			res := h.run(o, req)
			log := h.logContents(res.RunID)
			said := strings.Contains(log, "this machine's Xcode changed while the job ran (27.2 → 27.3)")
			if said != (after != "27.2") {
				t.Errorf("note said: %v, want %v:\n%s", said, after != "27.2", log)
			}
		})
	}
}
