package system

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// mkTree makes empty files (and their directories) under root; an entry
// ending in / is a directory, and one with an x: prefix is executable.
func mkTree(t *testing.T, root string, entries ...string) {
	t.Helper()
	for _, e := range entries {
		mode := os.FileMode(0o644)
		if after, ok := cutPrefix(e, "x:"); ok {
			e, mode = after, 0o755
		}
		p := filepath.Join(root, e)
		if e[len(e)-1] == '/' {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return s, false
}

// A Mac with two Xcodes, an iOS runtime, a Homebrew JDK and the Android
// command-line tools, all read from a fixture rather than the machine.
func TestToolchains_AMac(t *testing.T) {
	root, home, bin := t.TempDir(), t.TempDir(), t.TempDir()
	mkTree(t, root,
		"Applications/Xcode_26.0.1.app/Contents/Info.plist",
		"Applications/Xcode-27.app/Contents/Info.plist",
		"opt/homebrew/opt/openjdk@17/",
		"opt/homebrew/share/android-commandlinetools/platforms/android-36/",
		"opt/homebrew/share/android-commandlinetools/platforms/android-35/",
	)
	mkTree(t, bin, "x:xcodebuild", "x:xcodegen", "x:brew", "notexec")
	run := newFakeRunner(map[string]fakeCmd{
		"plutil -extract CFBundleShortVersionString raw -o - " + filepath.Join(root, "Applications/Xcode_26.0.1.app/Contents/Info.plist"): {out: "26.0.1\n"},
		"plutil -extract CFBundleShortVersionString raw -o - " + filepath.Join(root, "Applications/Xcode-27.app/Contents/Info.plist"):     {out: "27.0\n"},
		"xcrun simctl list runtimes -j": {out: `{"runtimes":[{"platform":"iOS","version":"26.0","buildversion":"23A","isAvailable":true}]}`},
		"/usr/libexec/java_home -V":     {exit: 1, out: "Unable to find any JVMs matching version \"(null)\".\n"},
		"sudo -n true":                  {exit: 1, out: "sudo: a password is required\n"},
	})
	s := New(Options{GOOS: "darwin", Home: home, Root: root, Run: run.run, Getenv: func(string) string { return "" }})

	tc := s.Toolchains(t.Context(), bin+":/nonexistent")
	if !tc.HasXcode("26") || !tc.HasXcode("27") || tc.HasXcode("25") || tc.XcodeVersions() != "26.0.1, 27.0" {
		t.Errorf("xcodes = %+v", tc.Xcodes)
	}
	if !tc.HasIOSRuntime() {
		t.Errorf("runtimes = %+v", tc.SimRuntimes)
	}
	if !slices.Equal(tc.JDKs, []int{17}) || !tc.HasJDK(17) || tc.HasJDK(21) {
		t.Errorf("jdks = %v", tc.JDKs)
	}
	if tc.AndroidSDK == "" || !slices.Equal(tc.AndroidPlatforms, []int{35, 36}) || tc.AndroidEmulator {
		t.Errorf("android = %q %v emulator %v", tc.AndroidSDK, tc.AndroidPlatforms, tc.AndroidEmulator)
	}
	if !tc.Tools["xcodebuild"] || !tc.Tools["xcodegen"] || tc.Tools["pod"] || tc.Tools["notexec"] {
		t.Errorf("tools = %v", tc.Tools)
	}
	if tc.PasswordlessSudo {
		t.Error("a sudo that wants a password read as passwordless")
	}
}

func TestToolchains_ALinuxBox(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	mkTree(t, root, "usr/lib/jvm/java-17-openjdk-amd64/", "usr/lib/jvm/java-21-openjdk-amd64/", "usr/lib/jvm/default-java/")
	mkTree(t, home, "Android/Sdk/platforms/android-34/", "x:Android/Sdk/emulator/emulator")
	s := New(Options{GOOS: "linux", Home: home, Root: root, Run: newFakeRunner(map[string]fakeCmd{"sudo -n true": {}}).run,
		Getenv: func(string) string { return "" }})
	tc := s.Toolchains(t.Context(), "")
	if len(tc.Xcodes) != 0 || !slices.Equal(tc.JDKs, []int{17, 21}) {
		t.Errorf("xcodes %v, jdks %v", tc.Xcodes, tc.JDKs)
	}
	if !slices.Equal(tc.AndroidPlatforms, []int{34}) || !tc.AndroidEmulator || !tc.PasswordlessSudo {
		t.Errorf("toolchains = %+v", tc)
	}
}
