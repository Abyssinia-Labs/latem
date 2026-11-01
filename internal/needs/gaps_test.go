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
	box := system.Toolchains{JDKs: []int{21}, AndroidSDK: "/sdk", AndroidPlatforms: []int{34}, Tools: map[string]bool{"sdkmanager": true}}
	gs := Gaps(r, box, ExecutorNative)
	if got := strings.Join(gapKeys(gs), " "); got != "jdk android-sdk android-emulator" {
		t.Fatalf("gaps = %s", got)
	}
	if gs[0].Have != "JDK 21" || gs[1].Fixes()[0] != `sdkmanager "platforms;android-36"` {
		t.Errorf("gaps = %+v", gs)
	}
	if gs := Gaps(r, box, ExecutorTart); len(gs) != 0 {
		t.Errorf("a tart runner's toolchain is in its VM; gaps = %v", gapKeys(gs))
	}
}

// A machine that says what it is gets that machine's commands: Homebrew is
// not on Arch, and a job's labels saying macOS does not change that. What
// wants sudo is a person's to type, and what a person's package manager
// cannot name is said in words.
func TestGaps_SpeakTheHostsPackageManager(t *testing.T) {
	r := Report{Platform: "macOS", Kind: KindAndroid, Requires: []Requirement{
		req(JDK, ""), req(Node, ""), req(Bundler, ""), req(Go, ""),
	}}
	for _, c := range []struct {
		host system.Host
		jdk  string
		node string
	}{
		{system.Host{OS: "Linux", Family: system.FamilyArch}, "sudo pacman -S --needed jdk17-openjdk", "sudo pacman -S --needed nodejs"},
		{system.Host{OS: "Linux", Family: system.FamilyDebian}, "sudo apt-get install -y openjdk-17-jdk", "sudo apt-get install -y nodejs"},
		{system.Host{OS: "Linux", Family: system.FamilyFedora}, "sudo dnf install -y java-17-openjdk-devel", "sudo dnf install -y nodejs"},
	} {
		gs := Gaps(r, system.Toolchains{Host: c.host, JDKs: []int{27}, Tools: map[string]bool{}}, ExecutorNative)
		if len(gs) != 4 {
			t.Fatalf("%s: gaps = %v", c.host.Family, gapKeys(gs))
		}
		if gs[0].Missing != "JDK 17" || gs[0].Have != "JDK 27" || len(gs[0].Fix) != 0 || gs[0].YourLines()[0] != c.jdk {
			t.Errorf("%s: jdk gap = %+v", c.host.Family, gs[0])
		}
		if gs[1].YourLines()[0] != c.node {
			t.Errorf("%s: node gap = %+v", c.host.Family, gs[1])
		}
	}

	gs := Gaps(r, system.Toolchains{Host: system.Host{OS: "Linux"}, Tools: map[string]bool{}}, ExecutorNative)
	if len(gs[0].Manual) != 1 || !strings.Contains(gs[0].Manual[0], "JDK 17") {
		t.Errorf("a distribution latem does not know: %+v", gs[0])
	}

	mac := Gaps(r, system.Toolchains{Host: system.Host{OS: "macOS"}, Tools: map[string]bool{}}, ExecutorNative)
	if mac[0].Fixes()[0] != "brew install openjdk@17" {
		t.Errorf("a Mac's jdk gap = %+v", mac[0])
	}
}

// An Android job that names no JDK still wants 17, whatever else is there.
func TestGaps_AnAndroidJobWithNoJDKNamed(t *testing.T) {
	r := Report{Kind: KindAndroid, Requires: []Requirement{req(JDK, "")}}
	if gs := Gaps(r, system.Toolchains{JDKs: []int{27}}, ExecutorNative); len(gs) != 1 || gs[0].Missing != "JDK 17" {
		t.Errorf("gaps = %+v", gs)
	}
	if gs := Gaps(r, system.Toolchains{JDKs: []int{17}}, ExecutorNative); len(gs) != 0 {
		t.Errorf("with 17: gaps = %+v", gs)
	}
	r.Kind = KindLinux
	if gs := Gaps(r, system.Toolchains{JDKs: []int{27}}, ExecutorNative); len(gs) != 0 {
		t.Errorf("a job that takes any JDK: gaps = %+v", gs)
	}
}

// An Android SDK is built up one step at a time, each needing the last.
func TestGaps_AnAndroidSDKOneStepAtATime(t *testing.T) {
	r := Report{Kind: KindAndroid, Requires: []Requirement{req(AndroidSDK, "36")}}
	arch := system.Host{OS: "Linux", Family: system.FamilyArch}
	jdk := []system.JDKInstall{{Major: 17, Home: "/jvm/17"}}
	tools := func() system.Toolchains {
		return system.Toolchains{Host: arch, AndroidSDK: "/sdk", SDKManager: "/sdk/sdkmanager", JDKHomes: jdk,
			Tools: map[string]bool{}}
	}
	gaps := func(t2 system.Toolchains) string {
		gs := Gaps(r, t2, ExecutorNative)
		if len(gs) == 0 {
			return ""
		}
		return strings.Join(append(append([]string{gs[0].Missing}, gs[0].Fixes()...), gs[0].YourLines()...), " | ")
	}

	if got := gaps(system.Toolchains{Host: arch, Tools: map[string]bool{}}); got != "the Android command-line tools | latem setup android-sdk" {
		t.Errorf("no SDK: %s", got)
	}
	noJDK := tools()
	noJDK.JDKHomes = nil
	if got := gaps(noJDK); got != "" {
		t.Errorf("sdkmanager runs on a JDK, so that gap comes first: %s", got)
	}
	tc := tools()
	if got := gaps(tc); got != "the Android SDK's licences accepted | env JAVA_HOME=/jvm/17 /sdk/sdkmanager --licenses" {
		t.Errorf("no licences: %s", got)
	}
	tc.AndroidLicensed = true
	if got := gaps(tc); got != "Android platform-tools | env JAVA_HOME=/jvm/17 /sdk/sdkmanager platform-tools" {
		t.Errorf("no platform-tools: %s", got)
	}
	tc.AndroidPlatformTools = true
	if got := gaps(tc); got != `Android platform 36 | env JAVA_HOME=/jvm/17 /sdk/sdkmanager "platforms;android-36"` {
		t.Errorf("no platform: %s", got)
	}
	tc.AndroidPlatforms = []int{36}
	if got := gaps(tc); got != "" {
		t.Errorf("complete: %s", got)
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

func TestInstallLine(t *testing.T) {
	arch := system.Host{OS: "Linux", Family: system.FamilyArch}
	for _, c := range []struct {
		host         system.Host
		key, version string
		want         string
	}{
		{arch, JDK, "17", "sudo pacman -S --needed jdk17-openjdk"},
		{arch, Ruby, "", "sudo pacman -S --needed ruby"},
		{arch, DockerDaemon, "", "sudo pacman -S --needed docker"},
		{system.Host{OS: "Linux", Family: system.FamilyDebian}, DockerDaemon, "", "sudo apt-get install -y docker.io"},
		{system.Host{OS: "macOS"}, JDK, "17", "brew install openjdk@17"},
		{system.Host{OS: "macOS"}, Ruby, "", "brew install ruby"},
		{system.Host{OS: "Linux"}, JDK, "17", ""},
		{arch, Xcode, "", ""},
	} {
		if got := InstallLine(c.host, c.key, c.version); got != c.want {
			t.Errorf("InstallLine(%+v, %s) = %q, want %q", c.host, c.key, got, c.want)
		}
	}
}
