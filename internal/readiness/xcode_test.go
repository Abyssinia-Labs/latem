package readiness

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

var studioHost = system.Host{OS: "macOS", Arch: "arm64"}

// A new Mac with Xcode installed and xcode-select on the Command Line Tools
// (A108): iOS is in the way of the person's sudo, and nothing asks for a
// simulator runtime yet.
func TestXcodeInstalledButNotSelected(t *testing.T) {
	tc := system.Toolchains{Host: studioHost,
		Xcodes:       []system.XcodeInstall{{Path: "/Applications/Xcode.app", Version: system.Version{Major: 27, Minor: 2, Text: "27.2"}}},
		DeveloperDir: "/Library/Developer/CommandLineTools"}
	in := Input{Machine: "studio", Host: studioHost, Toolchains: tc,
		Runners: []Runner{{Name: "macos", Executor: needs.ExecutorNative, Labels: []string{"macos", "xcode"}, Toolchains: tc}}}
	k := kind(t, Build(in), needs.KindIOS)
	it := item(t, k, needs.XcodeSelectedGap)
	if it.Soft || len(it.Run) != 0 || len(it.Yours) != 1 ||
		strings.Join(it.Yours[0], " ") != "sudo xcode-select -s /Applications/Xcode.app/Contents/Developer" {
		t.Errorf("item = %+v", it)
	}
	if len(k.Items) != 1 || k.State != Missing {
		t.Errorf("kind = %s, items %q", k.State, titles(k))
	}
}
