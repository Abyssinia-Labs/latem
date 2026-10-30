package agent

import (
	"slices"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// What a control plane is told a job requires (A61): what the machine must
// have, with a version only when it is one, once each.
func TestRequirementsOf(t *testing.T) {
	got := requirementsOf(needs.Report{Requires: []needs.Requirement{
		{Key: needs.Xcode, Version: "27"},
		{Key: needs.Bun, ProvidedBy: "oven-sh/setup-bun"},
		{Key: needs.JDK, Version: "${{ matrix.java }}"},
		{Key: needs.IOSSimulator},
		{Key: needs.IOSSimulator},
	}})
	want := []protocol.Requirement{{Key: "xcode", Version: "27"}, {Key: "jdk"}, {Key: "ios-simulator"}}
	if !slices.Equal(got, want) {
		t.Errorf("requirementsOf = %+v, want %+v", got, want)
	}
}

// The heartbeat's toolchains: versions and names, never a path.
func TestToolchainsAdvert(t *testing.T) {
	got := toolchainsAdvert(system.Toolchains{
		Xcodes:      []system.XcodeInstall{{Path: "/Applications/Xcode_27.app", Version: system.Version{Major: 27, Text: "27.0"}}},
		SimRuntimes: []system.SimRuntime{{Platform: "iOS", Available: true}},
		JDKs:        []int{17},
		AndroidSDK:  "/Users/me/Library/Android/sdk",
		Tools:       map[string]bool{"xcodegen": true, "docker": false, "bun": true},
	})
	if !slices.Equal(got.Xcode, []string{"27.0"}) || !got.IOSSimulator || !got.AndroidSDK ||
		!slices.Equal(got.Tools, []string{"bun", "xcodegen"}) || !slices.Equal(got.JDK, []int{17}) {
		t.Errorf("toolchainsAdvert = %+v", got)
	}
}
