package system

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileVault(t *testing.T) {
	// This is the check RUNNER_SPEC finding F13 exists for, so the table
	// covers the three answers that lead to three different conversations
	// with the operator — not just on and off.
	tests := []struct {
		name string
		goos string
		cmds map[string]fakeCmd
		want FileVaultStatus
	}{
		{"on", "darwin", map[string]fakeCmd{"fdesetup status": {out: "FileVault is On.\n"}}, FileVaultOn},
		{"off", "darwin", map[string]fakeCmd{"fdesetup status": {out: "FileVault is Off.\n"}}, FileVaultOff},
		{
			// Deferred enablement: encryption is on as far as the next reboot
			// is concerned, and the pre-boot screen will still appear.
			name: "on with deferred enablement",
			goos: "darwin",
			cmds: map[string]fakeCmd{"fdesetup status": {out: "FileVault is On.\nDeferred enablement appears to be active for user 'ci'.\n"}},
			want: FileVaultOn,
		},
		{"fdesetup absent", "darwin", nil, FileVaultUnknown},
		{"not macOS", "linux", nil, FileVaultUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem(tt.goos, newFakeRunner(tt.cmds), nil, "")
			got, _ := s.FileVault(context.Background())
			if got != tt.want {
				t.Errorf("FileVault = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFileVaultStatusString(t *testing.T) {
	for status, want := range map[FileVaultStatus]string{
		FileVaultOn:      "on",
		FileVaultOff:     "off",
		FileVaultUnknown: "unknown",
	} {
		if got := status.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", status, got, want)
		}
	}
}

func TestAutoLogin(t *testing.T) {
	const key = "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser"
	tests := []struct {
		name     string
		goos     string
		cmds     map[string]fakeCmd
		wantOn   bool
		wantUser string
	}{
		{"configured", "darwin", map[string]fakeCmd{key: {out: "ci\n"}}, true, "ci"},
		{
			// `defaults read` exits non-zero for an absent key and writes its
			// complaint to stderr. That is the answer, not a failure.
			name: "not configured",
			goos: "darwin",
			cmds: map[string]fakeCmd{key: {exit: 1, out: "The domain/default pair does not exist\n"}},
		},
		{"not macOS", "linux", nil, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem(tt.goos, newFakeRunner(tt.cmds), nil, "")
			got, err := s.AutoLogin(context.Background())
			if err != nil {
				t.Fatalf("AutoLogin: %v", err)
			}
			if got.Enabled != tt.wantOn || got.User != tt.wantUser {
				t.Errorf("AutoLogin = %+v, want enabled=%v user=%q", got, tt.wantOn, tt.wantUser)
			}
		})
	}
}

func TestLaunchContext(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want LaunchContext
	}{
		{"GUI session", "Aqua\n", LaunchContextAqua},
		{"ssh or pre-login", "Background\n", LaunchContextBackground},
		{"LaunchDaemon", "System\n", LaunchContextSystem},
		{"something else", "Wat\n", LaunchContextUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem("darwin", newFakeRunner(map[string]fakeCmd{"launchctl managername": {out: tt.out}}), nil, "")
			got, err := s.LaunchContext(context.Background())
			if err != nil {
				t.Fatalf("LaunchContext: %v", err)
			}
			if got != tt.want {
				t.Errorf("LaunchContext = %v, want %v", got, tt.want)
			}
			if got != LaunchContextUnknown && got.String() != tt.out[:len(tt.out)-1] {
				t.Errorf("String() = %q, want %q", got.String(), tt.out)
			}
		})
	}
}

func TestKeychainUnlocked(t *testing.T) {
	tests := []struct {
		name string
		goos string
		cmds map[string]fakeCmd
		want bool
	}{
		{"unlocked", "darwin", map[string]fakeCmd{"security show-keychain-info": {out: `Keychain "<NULL>" no-timeout`}}, true},
		{
			// A locked keychain after an unattended reboot is the single most
			// common cause of a signing job failing on a Mac, and the
			// error it produces names codesign rather than the keychain.
			name: "locked",
			goos: "darwin",
			cmds: map[string]fakeCmd{"security show-keychain-info": {exit: 36, out: "User interaction is not allowed."}},
		},
		{"not macOS", "linux", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem(tt.goos, newFakeRunner(tt.cmds), nil, "")
			got, _ := s.KeychainUnlocked(context.Background())
			if got != tt.want {
				t.Errorf("KeychainUnlocked = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestXcode(t *testing.T) {
	tests := []struct {
		name        string
		cmds        map[string]fakeCmd
		wantDir     string
		wantFull    bool
		wantLicense bool
		wantVersion string
	}{
		{
			name: "full Xcode, licence accepted",
			cmds: map[string]fakeCmd{
				"xcode-select -p":                    {out: "/Applications/Xcode.app/Contents/Developer\n"},
				"xcodebuild -checkFirstLaunchStatus": {},
				"xcodebuild -version":                {out: "Xcode 26.0.1\nBuild version 17A400\n"},
			},
			wantDir: "/Applications/Xcode.app/Contents/Developer", wantFull: true,
			wantLicense: true, wantVersion: "26.0.1",
		},
		{
			// The Command Line Tools cannot build an iOS app, and the failure
			// reads as a project problem rather than a machine one.
			name: "command line tools only",
			cmds: map[string]fakeCmd{
				"xcode-select -p": {out: "/Library/Developer/CommandLineTools\n"},
			},
			wantDir: "/Library/Developer/CommandLineTools",
		},
		{
			name: "licence not accepted",
			cmds: map[string]fakeCmd{
				"xcode-select -p":                    {out: "/Applications/Xcode.app/Contents/Developer\n"},
				"xcodebuild -checkFirstLaunchStatus": {exit: 69, out: "Agreeing to the Xcode/iOS license requires admin privileges"},
			},
			wantDir: "/Applications/Xcode.app/Contents/Developer", wantFull: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem("darwin", newFakeRunner(tt.cmds), nil, "")
			got := s.Xcode(context.Background())
			if got.DeveloperDir != tt.wantDir {
				t.Errorf("DeveloperDir = %q, want %q", got.DeveloperDir, tt.wantDir)
			}
			if got.IsFullXcode != tt.wantFull {
				t.Errorf("IsFullXcode = %v, want %v", got.IsFullXcode, tt.wantFull)
			}
			if got.LicenseAccepted != tt.wantLicense {
				t.Errorf("LicenseAccepted = %v, want %v", got.LicenseAccepted, tt.wantLicense)
			}
			if tt.wantVersion != "" && got.Version.String() != tt.wantVersion {
				t.Errorf("Version = %s, want %s", got.Version, tt.wantVersion)
			}
		})
	}
}

func TestXcodeOffMacOS(t *testing.T) {
	s := testSystem("linux", newFakeRunner(nil), nil, "")
	if got := s.Xcode(context.Background()); got.DeveloperDir != "" {
		t.Errorf("Xcode on linux = %+v, want zero", got)
	}
}

func TestTartHome(t *testing.T) {
	s := New(Options{GOOS: "darwin", Home: "/home/tester", Getenv: func(string) string { return "" }})
	if got, want := s.TartHome(), filepath.Join("/home/tester", ".tart"); got != want {
		t.Errorf("TartHome = %q, want %q", got, want)
	}
	s = New(Options{GOOS: "darwin", Home: "/home/tester", Getenv: func(k string) string {
		if k == "TART_HOME" {
			return "~/alt-tart"
		}
		return ""
	}})
	if got, want := s.TartHome(), filepath.Join("/home/tester", "alt-tart"); got != want {
		t.Errorf("TartHome with TART_HOME = %q, want %q", got, want)
	}
}

func TestParseTartList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"tart's json", `[{"Source":"local","Name":"latem-xcode-26","Disk":90,"State":"stopped"}]`, []string{"latem-xcode-26"}},
		{"empty array", `[]`, []string{}},
		{"not json", "NAME STATE\nfoo stopped\n", nil},
		{"unexpected shape", `{"vms":[]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTartList([]byte(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("parseTartList = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseTartList[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTartImage(t *testing.T) {
	home := t.TempDir()
	vms := filepath.Join(home, ".tart", "vms", "latem-xcode-26")
	if err := os.MkdirAll(vms, 0o755); err != nil {
		t.Fatal(err)
	}
	// 64 KiB of payload, so the measured size is non-zero on any filesystem
	// with a sane block size.
	if err := mustWrite(filepath.Join(vms, "disk.img"), string(make([]byte, 64<<10)), 0o600); err != nil {
		t.Fatal(err)
	}

	listing, err := json.Marshal([]map[string]any{{"Name": "latem-xcode-26"}})
	if err != nil {
		t.Fatal(err)
	}
	r := newFakeRunner(map[string]fakeCmd{"tart list --format json": {out: string(listing)}})
	s := New(Options{GOOS: "darwin", Home: home, Run: r.run, Getenv: func(string) string { return "" }})

	got := s.TartImage(context.Background(), "latem-xcode-26")
	if !got.Exists {
		t.Fatal("the image directory exists, so Exists should be true")
	}
	if !got.Measured || got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d measured=%v, want a positive measurement", got.SizeBytes, got.Measured)
	}

	// §13.2 says doctor reports the base image's real size rather than
	// assuming one; an image that is not there has no size to report.
	if absent := s.TartImage(context.Background(), "not-built"); absent.Exists {
		t.Errorf("TartImage(not-built) = %+v, want Exists false", absent)
	}
	if empty := s.TartImage(context.Background(), ""); empty.Exists || empty.Measured {
		t.Errorf("TartImage(\"\") = %+v, want zero", empty)
	}
}

func TestTartImagesFallsBackToDisk(t *testing.T) {
	// An operator whose `tart` binary is missing still wants to know whether
	// the 90 GB image is on the disk — it is the thing filling the machine.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".tart", "vms", "sonoma-base"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(Options{GOOS: "darwin", Home: home, Run: newFakeRunner(nil).run, Getenv: func(string) string { return "" }})

	names, err := s.TartImages(context.Background())
	if err != nil {
		t.Fatalf("TartImages: %v", err)
	}
	if len(names) != 1 || names[0] != "sonoma-base" {
		t.Errorf("TartImages = %v, want [sonoma-base]", names)
	}
}

func TestTartImagesNoTartAtAll(t *testing.T) {
	s := New(Options{GOOS: "darwin", Home: t.TempDir(), Run: newFakeRunner(nil).run, Getenv: func(string) string { return "" }})
	names, err := s.TartImages(context.Background())
	if err != nil {
		t.Fatalf("TartImages on a machine with no tart at all: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("TartImages = %v, want none", names)
	}
}

func TestDiskUsage(t *testing.T) {
	dir := t.TempDir()
	if err := mustWrite(filepath.Join(dir, "a"), string(make([]byte, 8<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mustWrite(filepath.Join(sub, "b"), string(make([]byte, 8<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hardlink to an existing file adds no blocks; counting it twice is
	// exactly the error that inflates a cloned Tart image's measured size.
	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "a-link")); err != nil {
		t.Fatal(err)
	}

	got, err := diskUsage(dir)
	if err != nil {
		t.Fatalf("diskUsage: %v", err)
	}
	if got < 16<<10 {
		t.Errorf("diskUsage = %d, want at least the 16 KiB written", got)
	}
	if got > 128<<10 {
		t.Errorf("diskUsage = %d, which suggests the hardlink was counted twice", got)
	}
}
