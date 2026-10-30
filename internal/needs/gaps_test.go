package needs

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/system"
)

func req(key, version string) Requirement {
	return Requirement{Key: key, Version: version, Strength: Invoked}
}

func gapKeys(gs []Gap) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Requirement.Key)
	}
	return out
}

// The Mac mini this was written against has Xcode 26.0.1; a job asking for
// 27 is named, with what is there.
func TestGaps_ANativeMac(t *testing.T) {
	mac := system.Toolchains{
		Xcodes: []system.XcodeInstall{{Version: system.Version{Major: 26, Text: "26.0.1"}}},
		Tools:  map[string]bool{"xcodegen": true, "brew": true},
	}
	r := Report{Platform: "macOS", Requires: []Requirement{
		req(Xcode, "27"), req(IOSSimulator, ""), req(XcodeGen, ""), req(CocoaPods, ""),
		{Key: Bun, Strength: Invoked, ProvidedBy: "oven-sh/setup-bun"}, req(Sudo, ""),
	}}
	gs := Gaps(r, mac, ExecutorNative)
	if got := strings.Join(gapKeys(gs), " "); got != "xcode ios-simulator cocoapods sudo" {
		t.Fatalf("gaps = %s", got)
	}
	if gs[0].Missing != "Xcode 27" || gs[0].Have != "Xcode 26.0.1" {
		t.Errorf("xcode gap = %+v", gs[0])
	}
	if gs[1].Fixes()[0] != "xcodebuild -downloadPlatform iOS" || gs[2].Fixes()[0] != "brew install cocoapods" {
		t.Errorf("fixes = %v, %v", gs[1].Fixes(), gs[2].Fixes())
	}

	mac.Xcodes = append(mac.Xcodes, system.XcodeInstall{Version: system.Version{Major: 27, Text: "27.0"}})
	mac.SimRuntimes = []system.SimRuntime{{Platform: "iOS", Version: "27.0", Available: true}}
	mac.PasswordlessSudo = true
	if gs := Gaps(r, mac, ExecutorNative); strings.Join(gapKeys(gs), " ") != "cocoapods" {
		t.Errorf("with Xcode 27 and a runtime, gaps = %v", gapKeys(gs))
	}
}

// A docker runner's needs are its image's, except what no image gives.
func TestGaps_ADockerRunner(t *testing.T) {
	r := Report{Platform: "Linux", Requires: []Requirement{req(DockerDaemon, ""), req(Go, ""), req(Services, "")}}
	gs := Gaps(r, system.Toolchains{Tools: map[string]bool{}}, ExecutorDocker)
	if got := strings.Join(gapKeys(gs), " "); got != "docker-daemon services" {
		t.Errorf("gaps = %s", got)
	}
	if !strings.Contains(gs[0].Manual[0], "native Linux runner") {
		t.Errorf("daemon gap = %+v", gs[0])
	}
}

func TestGaps_AndroidOnLinux(t *testing.T) {
	r := Report{Platform: "Linux", Requires: []Requirement{req(JDK, "17"), req(AndroidSDK, "36"), req(AndroidEmulator, "")}}
	box := system.Toolchains{JDKs: []int{21}, AndroidSDK: "/sdk", AndroidPlatforms: []int{34}}
	gs := Gaps(r, box, ExecutorNative)
	if got := strings.Join(gapKeys(gs), " "); got != "jdk android-sdk android-emulator" {
		t.Fatalf("gaps = %s", got)
	}
	if gs[0].Have != "JDK [21]" || gs[1].Fixes()[0] != `sdkmanager "platforms;android-36"` {
		t.Errorf("gaps = %+v", gs)
	}
	if gs := Gaps(r, box, ExecutorTart); len(gs) != 0 {
		t.Errorf("a tart runner's toolchain is in its VM; gaps = %v", gapKeys(gs))
	}
}

// A fix is an argv; what a person reads is the line they would type, quoted
// where a shell would read a character.
func TestCommandLine(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"brew", "install", "openjdk@17"}, "brew install openjdk@17"},
		{[]string{"sdkmanager", "platforms;android-36"}, `sdkmanager "platforms;android-36"`},
		{[]string{"echo", `a "b" $HOME`}, `echo "a \"b\" \$HOME"`},
	} {
		if got := CommandLine(c.argv); got != c.want {
			t.Errorf("CommandLine(%q) = %s, want %s", c.argv, got, c.want)
		}
	}
}
