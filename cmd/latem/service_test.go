package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

func TestServiceSpecUsesThisBinary(t *testing.T) {
	asHomebrews(t, "")
	home := t.TempDir()
	sys := system.New(system.Options{GOOS: "linux", Home: home, User: "tester"})

	spec, err := serviceSpec(sys, "")
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	// A service manager has no working directory to resolve a relative path
	// against, so an absolute one is not a nicety.
	if !filepath.IsAbs(spec.ExecPath) {
		t.Errorf("ExecPath = %q, want an absolute path", spec.ExecPath)
	}
	if spec.Home != home || spec.User != "tester" {
		t.Errorf("spec = %+v, want the injected home and user", spec)
	}
}

// A keg is deleted by the next `brew upgrade`, so the service starts
// Homebrew's link instead, which that upgrade moves to the new version (A78).
func TestServiceSpecUsesHomebrewsLink(t *testing.T) {
	asHomebrews(t, "/opt/homebrew/opt/latem/bin/latem")
	sys := system.New(system.Options{GOOS: "darwin", Home: t.TempDir(), User: "tester"})
	spec, err := serviceSpec(sys, "")
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	if spec.ExecPath != "/opt/homebrew/opt/latem/bin/latem" {
		t.Errorf("ExecPath = %q, want Homebrew's opt link", spec.ExecPath)
	}

	// --exec is the operator's choice, and it wins.
	spec, err = serviceSpec(sys, "/usr/local/bin/latem")
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	if spec.ExecPath != "/usr/local/bin/latem" {
		t.Errorf("ExecPath = %q, want the explicit path", spec.ExecPath)
	}
}

func TestServiceSpecExplicitExecPath(t *testing.T) {
	home := t.TempDir()
	sys := system.New(system.Options{GOOS: "linux", Home: home, User: "tester"})
	spec, err := serviceSpec(sys, "/opt/latem/bin/latem")
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	if spec.ExecPath != "/opt/latem/bin/latem" {
		t.Errorf("ExecPath = %q", spec.ExecPath)
	}
}

func TestStopTimeoutFollowsShutdownGrace(t *testing.T) {
	// A service manager that kills the agent partway through tearing down a
	// Tart VM leaks 90 GB, so the two timeouts have to move together.
	if got := system.StopTimeoutFor(2 * time.Hour); got != 2*time.Hour+system.StopMargin {
		t.Errorf("StopTimeoutFor(2h) = %v", got)
	}
	if got := system.StopTimeoutFor(0); got != system.DefaultStopTimeout {
		t.Errorf("StopTimeoutFor(0) = %v, want the §23 default", got)
	}
}

func TestPrintServiceReport(t *testing.T) {
	rep := system.ServiceReport{
		Path: "/home/t/.config/systemd/user/latem.service",
		Steps: []system.Step{
			{Desc: "reload", Argv: []string{"systemctl", "--user", "daemon-reload"}},
			{Desc: "unload previous", Argv: []string{"launchctl", "bootout", "x"}, Err: errors.New("nope"), Optional: true},
			{Desc: "enable", Argv: []string{"systemctl", "--user", "enable"}, Err: errors.New("failed"), Output: "Unit not found"},
		},
		Notes: []string{"a LaunchAgent needs a session"},
	}

	var b strings.Builder
	printServiceReport(&b, rep, false)
	out := b.String()

	// The step list is the deliverable: service management is the part of
	// installation most likely to half-succeed, and an operator who can see
	// which of six commands failed can finish the job by hand.
	for _, want := range []string{
		"systemctl --user daemon-reload",
		"[ok  ]",
		"[skip]", // an optional failure is not a fault
		"[FAIL]",
		"Unit not found",
		"a LaunchAgent needs a session",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}

	b.Reset()
	printServiceReport(&b, rep, true)
	if !strings.Contains(b.String(), "dry run") {
		t.Error("a dry run should say so")
	}
	// A dry run must not claim anything succeeded.
	if strings.Contains(b.String(), "[ok  ]") {
		t.Errorf("a dry run marked a step as done:\n%s", b.String())
	}
}

func TestPrintPruneReport(t *testing.T) {
	rep := cache.Report{
		BytesReclaimed:   3 << 30,
		VolumesEvicted:   []string{"latem-cache-linux-x64-pnpm"},
		WorkspacesGone:   []string{"run-17"},
		VMsReaped:        []string{"latem-run-19"},
		MirrorsCollected: []string{"myapp.git"},
	}
	var b strings.Builder
	printPruneReport(&b, rep)
	out := b.String()

	for _, want := range []string{"3.0 GiB", "latem-cache-linux-x64-pnpm", "run-17", "latem-run-19", "myapp.git"} {
		if !strings.Contains(out, want) {
			t.Errorf("prune report missing %q:\n%s", want, out)
		}
	}

	// An empty report says what it reclaimed and lists nothing else, rather
	// than printing four empty headings.
	b.Reset()
	printPruneReport(&b, cache.Report{})
	if strings.Count(b.String(), "\n") > 1 {
		t.Errorf("an empty prune printed headings:\n%s", b.String())
	}
}

func TestServiceCommandsExist(t *testing.T) {
	root := newRootCommand()
	var service *string
	for _, c := range root.Commands() {
		if c.Name() == "service" {
			have := map[string]bool{}
			for _, sub := range c.Commands() {
				have[sub.Name()] = true
			}
			for _, want := range []string{"install", "uninstall", "restart", "reload", "status"} {
				if !have[want] {
					t.Errorf("service %s is missing", want)
				}
			}
			n := c.Name()
			service = &n
		}
	}
	if service == nil {
		t.Fatal("no service command")
	}
}

func TestCacheSubcommandsExist(t *testing.T) {
	root := newRootCommand()
	for _, c := range root.Commands() {
		if c.Name() != "cache" {
			continue
		}
		have := map[string]bool{}
		for _, sub := range c.Commands() {
			have[sub.Name()] = true
		}
		// §19: latem cache ls | prune | rm <name>
		for _, want := range []string{"ls", "prune", "rm"} {
			if !have[want] {
				t.Errorf("cache %s is missing", want)
			}
		}
		return
	}
	t.Fatal("no cache command")
}
