package system

import (
	"context"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Version
		ok   bool
	}{
		// The three banners this package actually has to read. Each is
		// formatted differently, which is why the parser looks for the first
		// dotted run rather than a fixed position.
		{"git", "git version 2.50.1 (Apple Git-155)", Version{2, 50, 1, "2.50.1"}, true},
		{"docker", "Docker version 29.6.2, build dfc4efb", Version{29, 6, 2, "29.6.2"}, true},
		{"tart bare", "2.28.0", Version{2, 28, 0, "2.28.0"}, true},
		{"xcodebuild", "Xcode 26.0.1", Version{26, 0, 1, "26.0.1"}, true},

		{"two components", "git version 2.24", Version{2, 24, 0, "2.24"}, true},
		{"four components ignores the fourth", "tool 1.2.3.4", Version{1, 2, 3, "1.2.3"}, true},
		// A bare integer earlier in the line is skipped rather than accepted,
		// so a build number does not shadow the version behind it.
		{"leading noise", "Apple Git-155, git version 2.50.1", Version{2, 50, 1, "2.50.1"}, true},

		// A bare integer is not a version. Reading a build number or a year as
		// one produces a confident wrong answer in a minimum-version check,
		// which is worse than reporting that the version is unknown.
		{"bare integer", "Docker version 29", Version{}, false},
		{"no digits", "command not found", Version{}, false},
		{"empty", "", Version{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseVersion(tt.in)
			if ok != tt.ok {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v (got %+v)", tt.in, ok, tt.ok, got)
			}
			if !ok {
				return
			}
			if got.Major != tt.want.Major || got.Minor != tt.want.Minor || got.Patch != tt.want.Patch {
				t.Errorf("ParseVersion(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if tt.want.Text != "" && got.Text != tt.want.Text {
				t.Errorf("ParseVersion(%q).Text = %q, want %q", tt.in, got.Text, tt.want.Text)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	// 2.24 is the git release that introduced --end-of-options, which
	// RUNNER_SPEC amendment A7 requires for every argument position holding a
	// ref. The boundary cases below are the ones that decide whether a machine
	// gets INV-3's argument-injection defence.
	tests := []struct {
		name  string
		v     Version
		want  bool
		major int
		minor int
		patch int
	}{
		{"exactly the floor", Version{2, 24, 0, ""}, true, 2, 24, 0},
		{"one patch below", Version{2, 23, 9, ""}, false, 2, 24, 0},
		{"one minor above", Version{2, 25, 0, ""}, true, 2, 24, 0},
		{"major above", Version{3, 0, 0, ""}, true, 2, 24, 0},
		{"major below", Version{1, 99, 99, ""}, false, 2, 24, 0},
		{"patch floor met", Version{2, 24, 3, ""}, true, 2, 24, 3},
		{"patch floor missed", Version{2, 24, 2, ""}, false, 2, 24, 3},
		{"zero version fails every floor", Version{}, false, 2, 24, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.AtLeast(tt.major, tt.minor, tt.patch); got != tt.want {
				t.Errorf("%v.AtLeast(%d,%d,%d) = %v, want %v", tt.v, tt.major, tt.minor, tt.patch, got, tt.want)
			}
		})
	}
}

func TestVersionZeroAndString(t *testing.T) {
	if !(Version{}).IsZero() {
		t.Error("the zero Version should report IsZero")
	}
	if (Version{Major: 1}).IsZero() {
		t.Error("1.0.0 should not report IsZero")
	}
	if got := (Version{2, 50, 1, "2.50.1"}).String(); got != "2.50.1" {
		t.Errorf("String() = %q, want 2.50.1", got)
	}
}

func TestTool(t *testing.T) {
	tests := []struct {
		name       string
		onPath     []string
		cmds       map[string]fakeCmd
		probe      string
		args       []string
		wantFound  bool
		wantBanner string
		wantVer    string
		wantErr    bool
	}{
		{
			name:       "present with a readable banner",
			onPath:     []string{"git"},
			cmds:       map[string]fakeCmd{"git --version": {out: "git version 2.50.1 (Apple Git-155)\n"}},
			probe:      "git",
			wantFound:  true,
			wantBanner: "git version 2.50.1 (Apple Git-155)",
			wantVer:    "2.50.1",
		},
		{
			name:      "absent from PATH is not an error",
			probe:     "tart",
			wantFound: false,
		},
		{
			// A tool that exists but will not report a version is usually a
			// broken install or a shim waiting on something, and saying so is
			// more useful than reporting it missing.
			name:      "present but the version command fails",
			onPath:    []string{"docker"},
			cmds:      map[string]fakeCmd{"docker --version": {exit: 1}},
			probe:     "docker",
			wantFound: true,
			wantErr:   true,
		},
		{
			name:       "present with an unparsable banner",
			onPath:     []string{"weird"},
			cmds:       map[string]fakeCmd{"weird --version": {out: "the newest one\n"}},
			probe:      "weird",
			wantFound:  true,
			wantBanner: "the newest one",
		},
		{
			name:       "custom version arguments",
			onPath:     []string{"tart"},
			cmds:       map[string]fakeCmd{"tart --version": {out: "2.28.0"}},
			probe:      "tart",
			args:       []string{"--version"},
			wantFound:  true,
			wantBanner: "2.28.0",
			wantVer:    "2.28.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem("linux", newFakeRunner(tt.cmds), fakePath(tt.onPath...), "")
			got := s.Tool(context.Background(), tt.probe, tt.args...)
			if got.Found != tt.wantFound {
				t.Fatalf("Found = %v, want %v", got.Found, tt.wantFound)
			}
			if got.Banner != tt.wantBanner {
				t.Errorf("Banner = %q, want %q", got.Banner, tt.wantBanner)
			}
			if tt.wantVer != "" && got.Version.String() != tt.wantVer {
				t.Errorf("Version = %s, want %s", got.Version, tt.wantVer)
			}
			if (got.Err != nil) != tt.wantErr {
				t.Errorf("Err = %v, wantErr %v", got.Err, tt.wantErr)
			}
		})
	}
}

func TestToolConvenienceProbes(t *testing.T) {
	r := newFakeRunner(map[string]fakeCmd{
		"git --version":    {out: "git version 2.50.1"},
		"docker --version": {out: "Docker version 29.6.2, build dfc4efb"},
		"tart --version":   {out: "2.28.0"},
	})
	s := testSystem("darwin", r, fakePath("git", "docker", "tart"), "")
	ctx := context.Background()

	if v := s.Git(ctx).Version; !v.AtLeast(2, 24, 0) {
		t.Errorf("git version %v should satisfy the A7 floor", v)
	}
	if v := s.Docker(ctx).Version; v.Major != 29 {
		t.Errorf("docker major = %d, want 29", v.Major)
	}
	if v := s.Tart(ctx).Version; v.Major != 2 || v.Minor != 28 {
		t.Errorf("tart version = %v, want 2.28.x", v)
	}
}
