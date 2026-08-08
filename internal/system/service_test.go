package system

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden service definitions")

// goldenSpec is the reference installation: the binary where install.sh puts
// it, the default config directory, and RUNNER_SPEC §23's timeouts.
func goldenSpec() ServiceSpec {
	return ServiceSpec{
		ExecPath: "/home/tester/.local/bin/latem",
		Home:     "/home/tester",
		User:     "tester",
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a test fixture path built from a literal
	if err != nil {
		t.Fatalf("reading golden %s (run `go test ./internal/system -update`): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s does not match the golden file.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestSystemdUnitGolden(t *testing.T) {
	got, err := goldenSpec().SystemdUnit()
	if err != nil {
		t.Fatalf("SystemdUnit: %v", err)
	}
	checkGolden(t, "latem.service", got)

	// The three properties RUNNER_SPEC §23 states outright, asserted
	// separately from the golden file so a careless golden update cannot
	// quietly drop one.
	for _, want := range []string{
		"ExecStart=%h/.local/bin/latem agent",
		"TimeoutStopSec=1830",
		"WantedBy=default.target",
		"After=network-online.target docker.service",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit is missing %q:\n%s", want, got)
		}
	}
	// It is a *user* unit. Anything that would need root has no business here.
	if strings.Contains(got, "User=") || strings.Contains(got, "WantedBy=multi-user.target") {
		t.Errorf("unit looks like a system unit; §23 specifies a user unit:\n%s", got)
	}
}

func TestSystemdUnitWithConfigDir(t *testing.T) {
	spec := goldenSpec()
	spec.ConfigDir = "/home/tester/.latem-second"
	got, err := spec.SystemdUnit()
	if err != nil {
		t.Fatalf("SystemdUnit: %v", err)
	}
	// §8: a second agent on one machine relocates its lock, socket, database,
	// work directory, and caches together, and the environment variable is
	// what moves all of them at once.
	if !strings.Contains(got, "Environment=LATEM_CONFIG_DIR=%h/.latem-second") {
		t.Errorf("unit does not relocate the config dir:\n%s", got)
	}
}

func TestLaunchAgentPlistGolden(t *testing.T) {
	spec := goldenSpec()
	spec.ExecPath = "/Users/tester/.local/bin/latem"
	spec.Home = "/Users/tester"
	got, err := spec.LaunchAgentPlist()
	if err != nil {
		t.Fatalf("LaunchAgentPlist: %v", err)
	}
	checkGolden(t, "com.latem.agent.plist", got)

	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.latem.agent</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<integer>1830</integer>",
		// launchd throttles a Background job's CPU and disk, which makes an
		// Xcode build slower for reasons that appear in no log.
		"<string>Standard</string>",
		// launchd's minimal PATH contains no Homebrew prefix, so an agent
		// started at login cannot find tart or fastlane.
		"/opt/homebrew/bin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q:\n%s", want, got)
		}
	}
}

func TestPlistEscapesXML(t *testing.T) {
	spec := goldenSpec()
	spec.Home = "/Users/a&b"
	spec.ExecPath = "/Users/a&b/.local/bin/latem"
	got, err := spec.LaunchAgentPlist()
	if err != nil {
		t.Fatalf("LaunchAgentPlist: %v", err)
	}
	if strings.Contains(got, "a&b") {
		t.Errorf("ampersand reached the plist unescaped, which makes it unparsable:\n%s", got)
	}
	if !strings.Contains(got, "a&amp;b") {
		t.Errorf("expected an escaped ampersand:\n%s", got)
	}
}

func TestServiceSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    ServiceSpec
		wantErr bool
	}{
		{"complete", goldenSpec(), false},
		{"no binary", ServiceSpec{Home: "/home/tester"}, true},
		// A service manager has no working directory to resolve a relative
		// path against, so the failure would be at boot rather than here.
		{"relative binary", ServiceSpec{ExecPath: "bin/latem", Home: "/home/tester"}, true},
		{"no home", ServiceSpec{ExecPath: "/usr/local/bin/latem"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.spec.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHomeSpecifier(t *testing.T) {
	tests := []struct {
		name, path, home, want string
	}{
		{"under home", "/home/t/.local/bin/latem", "/home/t", "%h/.local/bin/latem"},
		{"outside home", "/usr/local/bin/latem", "/home/t", "/usr/local/bin/latem"},
		{"home itself", "/home/t", "/home/t", "/home/t"},
		{"no home known", "/opt/latem", "", "/opt/latem"},
		{"prefix collision", "/home/tester2/bin/latem", "/home/t", "/home/tester2/bin/latem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := homeSpecifier(tt.path, tt.home, "%h"); got != tt.want {
				t.Errorf("homeSpecifier(%q, %q) = %q, want %q", tt.path, tt.home, got, tt.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/usr/bin/latem", "/usr/bin/latem"},
		{"agent", "agent"},
		// systemd splits ExecStart on whitespace, so a path through a
		// directory with a space produces a unit that fails at boot naming
		// half of it.
		{"/home/a b/latem", `"/home/a b/latem"`},
		{`with"quote`, `"with\"quote"`},
		// systemd's %h expands inside quotes too, but leaving it bare keeps a
		// generated unit textually identical to the one in §23.
		{"%h/.local/bin/latem", "%h/.local/bin/latem"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := shellQuote(tt.in); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderPerPlatform(t *testing.T) {
	spec := goldenSpec()
	tests := []struct {
		goos     string
		wantPath string
		wantErr  bool
	}{
		{"darwin", filepath.Join("/home/tester", "Library", "LaunchAgents", "com.latem.agent.plist"), false},
		{"linux", filepath.Join("/home/tester", ".config", "systemd", "user", "latem.service"), false},
		{"windows", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			s := testSystem(tt.goos, newFakeRunner(nil), nil, "/home/tester")
			path, content, err := s.Render(spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Render on %s: err = %v, wantErr %v", tt.goos, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
			if content == "" {
				t.Error("rendered an empty service definition")
			}
		})
	}
}

func TestWriteService(t *testing.T) {
	home := t.TempDir()
	spec := goldenSpec()
	spec.Home = home
	spec.ExecPath = filepath.Join(home, ".local", "bin", "latem")

	s := testSystem("linux", newFakeRunner(nil), nil, home)
	path, err := s.WriteService(spec)
	if err != nil {
		t.Fatalf("WriteService: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	// A 0600 unit is a service that silently never starts: the service
	// manager cannot read it.
	if fi.Mode().Perm()&0o044 == 0 {
		t.Errorf("unit mode %v is unreadable by the service manager", fi.Mode().Perm())
	}
}

func TestInstallServiceDryRunLinux(t *testing.T) {
	r := newFakeRunner(nil)
	s := testSystem("linux", r, nil, "/home/tester")
	rep, err := s.InstallService(context.Background(), goldenSpec(), InstallOptions{DryRun: true})
	if err != nil {
		t.Fatalf("InstallService dry run: %v", err)
	}
	joined := stepText(rep)
	for _, want := range []string{
		"systemctl --user daemon-reload",
		// Without linger the unit stops the moment the operator logs out,
		// which on a headless build machine is seconds after the install.
		"loginctl enable-linger tester",
		"systemctl --user enable --now latem.service",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("install steps missing %q:\n%s", want, joined)
		}
	}
	if r.called("systemctl --user daemon-reload") {
		t.Error("a dry run ran a command")
	}
	if _, err := os.Stat(rep.Path); err == nil {
		t.Errorf("a dry run wrote %s", rep.Path)
	}
}

func TestInstallServiceDryRunMacOS(t *testing.T) {
	s := testSystem("darwin", newFakeRunner(nil), nil, "/Users/tester")
	rep, err := s.InstallService(context.Background(), ServiceSpec{
		ExecPath: "/Users/tester/.local/bin/latem",
		Home:     "/Users/tester",
		User:     "tester",
	}, InstallOptions{DryRun: true})
	if err != nil {
		t.Fatalf("InstallService dry run: %v", err)
	}
	joined := stepText(rep)
	for _, want := range []string{
		// gui/<uid>, never system/. §23's first requirement is that this is a
		// LaunchAgent: codesign reaches the login keychain from here and from
		// nowhere else.
		"launchctl bootstrap gui/501",
		"launchctl kickstart -k gui/501/com.latem.agent",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("install steps missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "system/") || strings.Contains(joined, "LaunchDaemons") {
		t.Errorf("install steps reach into the daemon domain:\n%s", joined)
	}
	if len(rep.Notes) == 0 {
		t.Error("a macOS install should say that a LaunchAgent needs a session")
	}
}

func TestInstallServiceSkips(t *testing.T) {
	s := testSystem("linux", newFakeRunner(nil), nil, "/home/tester")
	rep, err := s.InstallService(context.Background(), goldenSpec(), InstallOptions{
		DryRun: true, SkipLinger: true, SkipStart: true,
	})
	if err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	joined := stepText(rep)
	if strings.Contains(joined, "enable-linger") {
		t.Errorf("SkipLinger did not skip:\n%s", joined)
	}
	if strings.Contains(joined, "--now") {
		t.Errorf("SkipStart did not skip:\n%s", joined)
	}
}

func TestInstallServiceRunsCommands(t *testing.T) {
	home := t.TempDir()
	r := newFakeRunner(map[string]fakeCmd{
		"systemctl --user daemon-reload":              {},
		"loginctl enable-linger tester":               {},
		"systemctl --user enable --now latem.service": {},
	})
	s := testSystem("linux", r, nil, home)
	spec := goldenSpec()
	spec.Home = home

	rep, err := s.InstallService(context.Background(), spec, InstallOptions{})
	if err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	if _, statErr := os.Stat(rep.Path); statErr != nil {
		t.Fatalf("unit was not written: %v", statErr)
	}
	if !r.called("systemctl --user enable --now latem.service") {
		t.Error("the service was never enabled")
	}
}

func TestInstallServiceReportsFailures(t *testing.T) {
	home := t.TempDir()
	// daemon-reload succeeds, enable fails: the half-succeeded case that
	// makes the step list worth having.
	r := newFakeRunner(map[string]fakeCmd{
		"systemctl --user daemon-reload":              {},
		"loginctl enable-linger tester":               {},
		"systemctl --user enable --now latem.service": {exit: 1, out: "Failed to enable unit"},
	})
	s := testSystem("linux", r, nil, home)
	spec := goldenSpec()
	spec.Home = home

	rep, err := s.InstallService(context.Background(), spec, InstallOptions{})
	if err == nil {
		t.Fatal("a failing enable should be reported as an error")
	}
	if !strings.Contains(err.Error(), "systemctl --user enable") {
		t.Errorf("error does not name the failing command: %v", err)
	}
	if _, statErr := os.Stat(rep.Path); statErr != nil {
		t.Error("the unit should still have been written, so an operator can finish by hand")
	}
}

func TestOptionalStepFailureIsNotAnError(t *testing.T) {
	// launchctl refuses to bootstrap a label that is already loaded, so a
	// reinstall boots the old one out first — and that command fails on a
	// machine where nothing was loaded. It must not fail the install.
	home := t.TempDir()
	r := newFakeRunner(map[string]fakeCmd{
		"launchctl bootout gui/501/com.latem.agent":      {exit: 3, out: "No such process"},
		"launchctl bootstrap gui/501/" + "":              {},
		"launchctl enable gui/501/com.latem.agent":       {},
		"launchctl kickstart -k gui/501/com.latem.agent": {},
	})
	s := testSystem("darwin", r, nil, home)
	spec := goldenSpec()
	spec.Home = home
	spec.ExecPath = filepath.Join(home, ".local", "bin", "latem")

	rep, _ := s.InstallService(context.Background(), spec, InstallOptions{})
	var bootout Step
	for _, st := range rep.Steps {
		if len(st.Argv) > 1 && st.Argv[1] == "bootout" {
			bootout = st
		}
	}
	if bootout.Err == nil {
		t.Fatal("expected the bootout step to have failed in this fixture")
	}
	if !bootout.Optional {
		t.Error("bootout must be optional; a first install has nothing to unload")
	}
}

func TestUninstallService(t *testing.T) {
	home := t.TempDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(unitDir, SystemdUnitName)
	if err := mustWrite(unit, "[Unit]\n", 0o644); err != nil {
		t.Fatal(err)
	}

	r := newFakeRunner(map[string]fakeCmd{
		"systemctl --user disable --now latem.service": {},
		"systemctl --user daemon-reload":               {},
	})
	s := testSystem("linux", r, nil, home)
	if _, err := s.UninstallService(context.Background(), InstallOptions{}); err != nil {
		t.Fatalf("UninstallService: %v", err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("unit still present after uninstall: %v", err)
	}
}

func TestUninstallServiceIdempotent(t *testing.T) {
	// Removing a service that was never installed is not a failure; an
	// operator retrying an install should not have to reason about it.
	s := testSystem("linux", newFakeRunner(map[string]fakeCmd{
		"systemctl --user disable --now latem.service": {},
		"systemctl --user daemon-reload":               {},
	}), nil, t.TempDir())
	if _, err := s.UninstallService(context.Background(), InstallOptions{}); err != nil {
		t.Errorf("uninstalling an absent service: %v", err)
	}
}

func TestServiceStatus(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		cmds        map[string]fakeCmd
		installed   bool
		wantRunning bool
	}{
		{
			name:        "linux active",
			goos:        "linux",
			cmds:        map[string]fakeCmd{"systemctl --user is-active latem.service": {out: "active\n"}},
			installed:   true,
			wantRunning: true,
		},
		{
			name:      "linux installed but stopped",
			goos:      "linux",
			cmds:      map[string]fakeCmd{"systemctl --user is-active latem.service": {exit: 3, out: "inactive\n"}},
			installed: true,
		},
		{
			name:        "macOS running",
			goos:        "darwin",
			cmds:        map[string]fakeCmd{"launchctl print gui/501/com.latem.agent": {out: "com.latem.agent = {\n\tstate = running\n"}},
			installed:   true,
			wantRunning: true,
		},
		{
			// "not running" contains "running", so a careless substring check
			// reports a stopped agent as live.
			name:      "macOS loaded but stopped",
			goos:      "darwin",
			cmds:      map[string]fakeCmd{"launchctl print gui/501/com.latem.agent": {out: "com.latem.agent = {\n\tstate = not running\n"}},
			installed: true,
		},
		{
			name: "macOS not loaded",
			goos: "darwin",
			cmds: map[string]fakeCmd{"launchctl print gui/501/com.latem.agent": {exit: 113, out: "Could not find service"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			s := testSystem(tt.goos, newFakeRunner(tt.cmds), nil, home)
			if tt.installed {
				path, _, err := s.Render(goldenSpec())
				if err != nil {
					t.Fatal(err)
				}
				path = strings.Replace(path, "/home/tester", home, 1)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := mustWrite(path, "x", 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.ServiceStatus(context.Background())
			if err != nil {
				t.Fatalf("ServiceStatus: %v", err)
			}
			if got.Running != tt.wantRunning {
				t.Errorf("Running = %v, want %v (detail %q)", got.Running, tt.wantRunning, got.Detail)
			}
		})
	}
}

func TestServiceStatusUnsupported(t *testing.T) {
	s := testSystem("windows", newFakeRunner(nil), nil, "")
	if _, err := s.ServiceStatus(context.Background()); err == nil {
		t.Error("expected an error on a platform with no service integration")
	}
}

func TestServiceSpecDefaults(t *testing.T) {
	spec := ServiceSpec{ExecPath: "/opt/latem", Home: "/home/tester"}.normalize()
	if spec.StopTimeout != DefaultStopTimeout {
		t.Errorf("StopTimeout = %v, want %v", spec.StopTimeout, DefaultStopTimeout)
	}
	if spec.RestartDelay != DefaultRestartDelay {
		t.Errorf("RestartDelay = %v, want %v", spec.RestartDelay, DefaultRestartDelay)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "agent" {
		t.Errorf("Args = %v, want [agent]", spec.Args)
	}
	if spec.LogDir != filepath.Join("/home/tester", ".latem", "logs") {
		t.Errorf("LogDir = %q", spec.LogDir)
	}
	// 1830 seconds is shutdown_grace plus a margin. Too short kills the agent
	// while it is tearing down VMs, and a leaked one costs 90 GB.
	if DefaultStopTimeout != 1830*time.Second {
		t.Errorf("DefaultStopTimeout = %v, want 1830s per §23", DefaultStopTimeout)
	}
}

func stepText(r ServiceReport) string {
	var b strings.Builder
	for _, s := range r.Steps {
		b.WriteString(s.String())
		b.WriteByte('\n')
	}
	return b.String()
}
