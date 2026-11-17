package readiness

import (
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// studio is the Mac Studio of 2026-11-17: a macos runner as `latem init`
// writes it, and take-one's macOS release asking for a Blacksmith label that
// no runner here carries.
func studio() Input {
	h := system.Host{OS: "macOS", Arch: "arm64"}
	tc := system.Toolchains{Host: h, Xcodes: []system.XcodeInstall{{Path: "/Applications/Xcode.app", Version: system.Version{Major: 27, Text: "27.2"}}}}
	return Input{Machine: "studio", Host: h, Toolchains: tc,
		Runners: []Runner{{Name: "macos", Executor: needs.ExecutorNative, Labels: []string{"macos", "arm64", "xcode"}, Toolchains: tc}},
		Jobs: []Job{{Ref: "take-one release.yml mac", Repo: "take-one", Labels: []string{"blacksmith-6vcpu-macos-26"},
			Report: needs.Report{Platform: "macOS", Kind: needs.KindMacOSApp, Requires: []needs.Requirement{req(needs.Xcode)}}}}}
}

// macOS apps has no label of its own, so the line that turns it on is the
// labels its jobs ask for, on the runner that would take them. It is named,
// and is a person's yes at a terminal (A107).
func TestRunsOn_AnOffKindWithNoLabelNamesTheLine(t *testing.T) {
	k := kind(t, Build(studio()), needs.KindMacOSApp)
	if k.State != Off || k.On {
		t.Fatalf("state = %s", k.State)
	}
	it := item(t, k, ItemRunsOn)
	if it.Soft || len(it.Config) != 1 {
		t.Fatalf("item = %+v", it)
	}
	if c := it.Config[0]; c.What != ChangeRunsOnLabel || c.Runner != "macos" || c.Value != "blacksmith-6vcpu-macos-26" {
		t.Errorf("change = %+v", c)
	}
	if !strings.Contains(it.Warning, `labels = ["macos", "arm64", "xcode", "blacksmith-6vcpu-macos-26"]`) {
		t.Errorf("warning = %q", it.Warning)
	}
	if !slices.Equal(it.Jobs, []string{"take-one release.yml mac"}) {
		t.Errorf("jobs = %q", it.Jobs)
	}
}

// A job that reaches a runner already needs no label, and a kind with a
// label of its own is switched by that label instead.
func TestRunsOn_NothingWhenTheJobsComeHere(t *testing.T) {
	in := studio()
	in.Jobs[0].Runner = "macos"
	k := kind(t, Build(in), needs.KindMacOSApp)
	if slices.ContainsFunc(k.Items, func(it Item) bool { return it.ID == ItemRunsOn }) {
		t.Errorf("items = %q", titles(k))
	}
	if k := kind(t, Build(desktop()), needs.KindAndroid); slices.ContainsFunc(k.Items, func(it Item) bool { return it.ID == ItemRunsOn }) {
		t.Errorf("android items = %q", titles(k))
	}
}
