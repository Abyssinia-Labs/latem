package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// FileVaultStatus is what `fdesetup status` reported.
type FileVaultStatus int

// FileVault states. Unknown is the zero value and means the question could not
// be answered — on Linux, or when `fdesetup` is unavailable — which is a
// different thing from "off" and must not be reported as reassurance.
const (
	FileVaultUnknown FileVaultStatus = iota
	FileVaultOff
	FileVaultOn
)

// String renders the status for display.
func (f FileVaultStatus) String() string {
	switch f {
	case FileVaultOn:
		return "on"
	case FileVaultOff:
		return "off"
	default:
		return "unknown"
	}
}

// FileVault reports whether full-disk encryption is enabled.
//
// This is the check RUNNER_SPEC finding F13 exists for, and the reason it is
// worth a dedicated probe rather than a line in a list. With FileVault on, an
// unattended reboot stops at the pre-boot unlock screen: no login, no session,
// no LaunchAgent, no builds, and nothing in any log to explain it, because the
// agent never started to write one. The symptom an operator sees is "my
// machine went offline overnight" and there is no evidence anywhere pointing
// at the cause.
//
// This package reports the fact. It does not decide what to do about it —
// see the caller, which names both options, because a machine holding signing
// certificates is a reasonable thing to want encrypted and the trade is the
// operator's to make.
func (s *System) FileVault(ctx context.Context) (FileVaultStatus, error) {
	if !s.IsMacOS() {
		return FileVaultUnknown, nil
	}
	out, err := s.Exec(ctx, "fdesetup", "status")
	text := strings.ToLower(string(out))
	switch {
	case strings.Contains(text, "filevault is on"):
		return FileVaultOn, nil
	case strings.Contains(text, "filevault is off"):
		return FileVaultOff, nil
	}
	if err != nil {
		return FileVaultUnknown, err
	}
	return FileVaultUnknown, nil
}

// AutoLogin is whether this machine logs a user in without a password at boot.
type AutoLogin struct {
	Enabled bool
	// User is the account that auto-logs in. It may differ from the account
	// the agent runs as, which looks like a working configuration and is not:
	// the LaunchAgent belongs to a user who is never logged in.
	User string
}

// AutoLogin reports the configured auto-login user.
//
// §23 requires it: a LaunchAgent needs a session, and after an unattended
// reboot a session only exists if somebody, or something, logged in.
func (s *System) AutoLogin(ctx context.Context) (AutoLogin, error) {
	if !s.IsMacOS() {
		return AutoLogin{}, nil
	}
	out, err := s.Exec(ctx, "defaults", "read",
		"/Library/Preferences/com.apple.loginwindow", "autoLoginUser")
	if err != nil {
		// `defaults read` on an absent key exits non-zero. That is the answer,
		// not a failure: auto-login is not configured.
		if ExitCode(err) > 0 {
			return AutoLogin{}, nil
		}
		return AutoLogin{}, err
	}
	name := firstLine(out)
	if name == "" {
		return AutoLogin{}, nil
	}
	return AutoLogin{Enabled: true, User: name}, nil
}

// LaunchContext is the launchd domain the current process belongs to.
type LaunchContext int

// Launch contexts, as `launchctl managername` reports them.
const (
	// LaunchContextUnknown means the question could not be answered.
	LaunchContextUnknown LaunchContext = iota
	// LaunchContextAqua is a GUI login session. This is the one §13.3 and §23
	// require: `codesign` reaches the login keychain from here and nowhere
	// else.
	LaunchContextAqua
	// LaunchContextBackground is a user domain with no GUI session — an ssh
	// login, or a LaunchAgent loaded before login completed.
	LaunchContextBackground
	// LaunchContextSystem is the LaunchDaemon domain. Signing fails here with
	// errors that point nowhere useful, which is why §23 states the
	// LaunchAgent requirement first among its three.
	LaunchContextSystem
)

// String renders the context as launchctl spells it.
func (c LaunchContext) String() string {
	switch c {
	case LaunchContextAqua:
		return "Aqua"
	case LaunchContextBackground:
		return "Background"
	case LaunchContextSystem:
		return "System"
	default:
		return "unknown"
	}
}

// LaunchContext reports which launchd domain this process is running in.
func (s *System) LaunchContext(ctx context.Context) (LaunchContext, error) {
	if !s.IsMacOS() {
		return LaunchContextUnknown, nil
	}
	out, err := s.Exec(ctx, "launchctl", "managername")
	if err != nil {
		return LaunchContextUnknown, err
	}
	switch strings.TrimSpace(firstLine(out)) {
	case "Aqua":
		return LaunchContextAqua, nil
	case "Background":
		return LaunchContextBackground, nil
	case "System":
		return LaunchContextSystem, nil
	default:
		return LaunchContextUnknown, nil
	}
}

// KeychainUnlocked reports whether the login keychain can be read right now.
//
// `security show-keychain-info` is used because it does not prompt and does
// not need a password: it either answers with the keychain's settings or fails
// with "User interaction is not allowed", which is exactly the state that
// turns every `codesign` in a signing job into a failure nobody attributes to
// a locked keychain (§18.1).
func (s *System) KeychainUnlocked(ctx context.Context) (bool, error) {
	if !s.IsMacOS() {
		return false, nil
	}
	_, err := s.Exec(ctx, "security", "show-keychain-info")
	if err == nil {
		return true, nil
	}
	if ExitCode(err) > 0 {
		return false, nil
	}
	return false, err
}

// Xcode is what the developer toolchain looks like on this machine.
type Xcode struct {
	// DeveloperDir is `xcode-select -p`.
	DeveloperDir string
	// IsFullXcode reports whether DeveloperDir points inside an Xcode.app
	// rather than at the standalone Command Line Tools. The Command Line
	// Tools cannot build an iOS app, and the failure — "unable to find a
	// destination matching the provided destination specifier" — reads as a
	// project problem rather than a machine one.
	IsFullXcode bool
	// LicenseAccepted is `xcodebuild -checkFirstLaunchStatus` succeeding.
	// An unaccepted licence fails every build on the machine with a message
	// that only appears on stderr of the first tool to notice.
	LicenseAccepted bool
	// Version is xcodebuild's reported version, when it would report one.
	Version Version
	// Err is why the toolchain could not be inspected.
	Err error
}

// Xcode inspects the selected developer toolchain.
func (s *System) Xcode(ctx context.Context) Xcode {
	var x Xcode
	if !s.IsMacOS() {
		return x
	}
	out, err := s.Exec(ctx, "xcode-select", "-p")
	if err != nil {
		x.Err = err
		return x
	}
	x.DeveloperDir = firstLine(out)
	x.IsFullXcode = strings.Contains(x.DeveloperDir, ".app/Contents/Developer")

	// -checkFirstLaunchStatus exits 0 only when the licence is agreed and the
	// first-launch tasks have run. It is quiet on success, which is what makes
	// it usable as a probe.
	if _, err := s.Exec(ctx, "xcodebuild", "-checkFirstLaunchStatus"); err == nil {
		x.LicenseAccepted = true
	}
	if out, err := s.Exec(ctx, "xcodebuild", "-version"); err == nil {
		if v, ok := ParseVersion(firstLine(out)); ok {
			x.Version = v
		}
	}
	return x
}

// TartImage is a Tart base image on disk.
type TartImage struct {
	Name string
	// Exists reports whether the image is present locally.
	Exists bool
	// SizeBytes is the space it actually occupies, measured by walking the
	// image directory and summing allocated blocks.
	//
	// Allocated rather than apparent, and measured rather than taken from
	// `tart list`, because a Tart disk image is sparse and an APFS clone
	// shares blocks with its parent. §13.2 says `doctor` "reports the base
	// image's real size rather than assuming this number", and the apparent
	// size of a sparse 90 GB image is the assumption, not the measurement.
	SizeBytes int64
	// Measured is false when the size could not be established, in which case
	// SizeBytes is meaningless rather than zero.
	Measured bool
	Err      error
}

// TartHome is where Tart keeps its VMs, honouring TART_HOME.
func (s *System) TartHome() string {
	if h := s.getenv("TART_HOME"); h != "" {
		return s.Expand(h)
	}
	return filepath.Join(s.home, ".tart")
}

// TartImages lists the local VM images tart knows about.
//
// It prefers `tart list --format json` and falls back to reading the images
// directory, because the two disagree in a way worth tolerating: tart's JSON
// schema is not a stable interface, and an operator whose `tart` binary is
// missing still wants to know whether the 90 GB image is on the disk.
func (s *System) TartImages(ctx context.Context) ([]string, error) {
	out, err := s.Exec(ctx, "tart", "list", "--format", "json")
	if err == nil {
		if names := parseTartList(out); names != nil {
			return names, nil
		}
	}
	dir := filepath.Join(s.TartHome(), "vms")
	entries, dirErr := os.ReadDir(dir)
	if dirErr != nil {
		if errors.Is(dirErr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, dirErr
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func parseTartList(out []byte) []string {
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if n, ok := r["Name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TartImage measures one Tart base image.
func (s *System) TartImage(ctx context.Context, name string) TartImage {
	img := TartImage{Name: name}
	if name == "" {
		return img
	}
	names, err := s.TartImages(ctx)
	if err != nil {
		img.Err = err
	}
	for _, n := range names {
		if n == name {
			img.Exists = true
			break
		}
	}
	dir := filepath.Join(s.TartHome(), "vms", name)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		img.Exists = true
		if size, err := diskUsage(dir); err == nil {
			img.SizeBytes, img.Measured = size, true
		} else {
			img.Err = err
		}
	}
	return img
}
