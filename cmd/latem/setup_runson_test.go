package main

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// take-one's macOS release, asking for a label no runner here carries and
// whose name says no runner: which one should carry it is the label's to
// say. A vendor's label that names one, as studio-release does,
// is an alias's to bring here instead (A101).
const macRelease = `on: push
jobs:
  mac:
    runs-on: studio-release
    steps:
      - run: xcodebuild archive -scheme Mac -destination 'generic/platform=macOS' -archivePath out
      - run: xcrun notarytool submit out.zip --wait
`

// Off a terminal, `--kind macos-app` names the label and the line, and
// leaves it to a person at one (A107), as it does the two settings.
func TestSetupApply_MacOSAppsOffNamesTheLabel(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/release.yml": {Data: []byte(macRelease)}})
	noAgentRunning(t)
	before := readConfigFile(t, dir)

	_, stderr, err := applyIn(t, dir, "--kind", "macos-app")
	if exitCode(err) != ExitNoTerminal {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	if readConfigFile(t, dir) != before {
		t.Error("it wrote config.toml off a terminal")
	}
	for _, want := range []string{
		"runs-on-labels: it gives something up, so only a person at a terminal may write it",
		"  it writes the macos runner gains the label studio-release",
		"  latem setup apply runs-on-labels --kind macos-app --config-dir " + shellWord(dir),
		"The line in its [[runner]] table becomes: labels = [",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "macos-app: a person's to do: macOS apps comes with") {
		t.Errorf("the dead end is back:\n%s", stderr)
	}
}

// At a terminal it is asked, and a yes puts the label on the runner, which
// brings the kind's jobs here.
func TestSetupApply_MacOSAppsAtATerminal(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/release.yml": {Data: []byte(macRelease)}})
	noAgentRunning(t)
	prev := global.configDir
	t.Cleanup(func() { global.configDir = prev })

	screen, stderr, err := atTerminal(t, terminalSession{prompt: "Write this to config.toml? [y/N]", answer: "y"},
		"setup", "apply", "runs-on-labels", "--kind", "macos-app", "--config-dir", dir)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, screen, stderr)
	}
	r, _ := loadConfigIn(t, dir).Runner("macos")
	if !slices.Contains(r.Labels, "studio-release") || !strings.Contains(screen, "will come to the macos runner") ||
		!strings.Contains(stderr, "runs-on-labels: wrote the macos runner gains the label studio-release; closed") {
		t.Errorf("labels %q\nscreen:\n%s\nstderr:\n%s", r.Labels, screen, stderr)
	}
}

// The Mac Studio's own label, blacksmith-6vcpu-macos-26, names the macos
// runner, so its fix is the alias and not a label on the runner (A101).
func TestSetup_AVendorsMacLabelIsTheAliasesNotTheRunners(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/release.yml": {Data: []byte(
		strings.ReplaceAll(macRelease, "studio-release", "blacksmith-6vcpu-macos-26"))}})
	noAgentRunning(t)

	_, stderr, err := applyIn(t, dir, "--kind", "macos-app")
	if exitCode(err) != ExitNoTerminal || strings.Contains(stderr, "runs-on-labels") ||
		!strings.Contains(stderr, "latem setup apply label-alias-blacksmith-6vcpu-macos-26 --kind macos-app") {
		t.Errorf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
}
