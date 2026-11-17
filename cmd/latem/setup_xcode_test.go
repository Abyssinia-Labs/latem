package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// iosItems is the ids of iOS's items in `latem setup --json` in dir.
func iosItems(t *testing.T, dir string) []string {
	t.Helper()
	out, err := latemIn(t, dir, "", "setup", "--json")
	if err != nil {
		t.Fatalf("setup --json: %v\n%s", err, out)
	}
	var plan readiness.Plan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var ids []string
	for _, k := range plan.Kinds {
		if k.Name != needs.KindIOS {
			continue
		}
		for _, it := range k.Items {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// Xcode installed and not selected is in iOS's way (A108), unless the runner
// says DEVELOPER_DIR itself, which a step gets before xcode-select's.
func TestSetup_XcodeNotSelectedUnlessTheRunnerSaysWhichXcode(t *testing.T) {
	dir, rec := setupMachine(t, nil)
	rec.machine.tc.Xcodes = []system.XcodeInstall{{Path: "/Applications/Xcode.app", Version: system.Version{Major: 27, Minor: 2, Text: "27.2"}}}
	rec.machine.tc.DeveloperDir = "/Library/Developer/CommandLineTools"
	if ids := iosItems(t, dir); !slices.Contains(ids, needs.XcodeSelectedGap) {
		t.Fatalf("ios items = %q", ids)
	}

	useConfigDir(t, dir)
	if err := applyChanges([]readiness.Change{{What: readiness.ChangeEnv, Runner: "macos", Key: "DEVELOPER_DIR",
		Value: "/Applications/Xcode.app/Contents/Developer"}}); err != nil {
		t.Fatal(err)
	}
	if ids := iosItems(t, dir); slices.Contains(ids, needs.XcodeSelectedGap) {
		t.Errorf("with DEVELOPER_DIR, ios items = %q", ids)
	}
}

// doctor names the Xcode that is installed, and the sudo is the person's.
// With none installed, selecting one is no fix.
func TestDoctor_XcodeInstalledButNotSelected(t *testing.T) {
	dir := privateDir(t)
	root := t.TempDir()
	plist := filepath.Join(root, "Applications", "Xcode-27.2.app", "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDoctor("darwin", dir, nil, testConfig(dir), nil)
	d.sys = system.New(system.Options{GOOS: "darwin", Home: dir, Root: root, Getenv: func(string) string { return "" },
		Run: newFakeRunner(map[string]fakeCmd{
			"plutil -extract CFBundleShortVersionString raw -o - " + plist: {out: "27.2\n"},
		}).run})
	c := d.xcodeNotSelected(t.Context(), "/Library/Developer/CommandLineTools")
	want := "sudo xcode-select -s " + filepath.Join(root, "Applications", "Xcode-27.2.app") + "/Contents/Developer"
	if c.Level != levelFail || !strings.HasPrefix(c.Detail, "installed but not selected: ") || !slices.Equal(c.Fix, []string{want}) ||
		!strings.Contains(c.Detail, "Xcode 27.2.0") || !strings.Contains(strings.Join(c.Notes, " "), "Yours to type") {
		t.Errorf("check = %+v", c)
	}

	d.sys = system.New(system.Options{GOOS: "darwin", Home: dir, Root: t.TempDir(), Getenv: func(string) string { return "" },
		Run: newFakeRunner(nil).run})
	c = d.xcodeNotSelected(t.Context(), "/Library/Developer/CommandLineTools")
	if len(c.Fix) != 0 || len(c.Manual) == 0 || !strings.Contains(c.Detail, "no Xcode is installed") {
		t.Errorf("no Xcode: check = %+v", c)
	}
}
