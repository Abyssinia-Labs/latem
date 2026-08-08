package system

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	s := New(Options{})
	if s.OS() == "" || s.Arch() == "" {
		t.Errorf("New(Options{}) left the platform empty: %s/%s", s.OS(), s.Arch())
	}
	if s.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", s.timeout, DefaultTimeout)
	}
	if s.run == nil || s.lookPath == nil || s.getenv == nil || s.getuid == nil {
		t.Error("New left an injected dependency nil, so a probe would panic")
	}
}

func TestPlatformPredicates(t *testing.T) {
	mac := New(Options{GOOS: "darwin"})
	lin := New(Options{GOOS: "linux"})
	if !mac.IsMacOS() || mac.IsLinux() {
		t.Error("darwin misclassified")
	}
	if !lin.IsLinux() || lin.IsMacOS() {
		t.Error("linux misclassified")
	}
}

func TestExpand(t *testing.T) {
	s := New(Options{Home: "/home/tester"})
	tests := []struct{ in, want string }{
		{"~", "/home/tester"},
		{"~/.latem/config.toml", "/home/tester/.latem/config.toml"},
		{"/etc/latem", "/etc/latem"},
		{"", ""},
		// Not a home reference: launchd and systemd both take this literally,
		// and expanding it would silently rewrite another user's path.
		{"~other/x", "~other/x"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := s.Expand(tt.in); got != tt.want {
				t.Errorf("Expand(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExecRunner(t *testing.T) {
	ctx := context.Background()

	out, err := ExecRunner(ctx, "echo", "hello")
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("echo produced %q", out)
	}

	// A command that runs and fails carries its exit code and its own words.
	_, err = ExecRunner(ctx, "sh", "-c", "echo nope >&2; exit 7")
	if got := ExitCode(err); got != 7 {
		t.Errorf("ExitCode = %d, want 7", got)
	}
	if !strings.Contains(string(Output(err)), "nope") {
		t.Errorf("Output = %q, want the command's stderr", Output(err))
	}

	// A command that does not exist is a different diagnosis with a different
	// fix, and must be distinguishable.
	_, err = ExecRunner(ctx, "latem-definitely-not-a-real-binary")
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("expected a CommandError, got %T", err)
	}
	if !ce.NotInstalled() {
		t.Errorf("NotInstalled = false for an absent binary: %v", ce.Err)
	}
	if ce.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a command that never ran", ce.ExitCode)
	}
	if ce.Error() == "" || !strings.Contains(ce.Error(), "latem-definitely") {
		t.Errorf("Error() = %q, should name the program", ce.Error())
	}
}

func TestExitCodeAndOutputOnOtherErrors(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
	if got := ExitCode(errors.New("something else")); got != -1 {
		t.Errorf("ExitCode(other) = %d, want -1", got)
	}
	if Output(errors.New("something else")) != nil {
		t.Error("Output(other) should be nil")
	}
}

func TestCommandErrorUnwrap(t *testing.T) {
	ce := &CommandError{Name: "git", ExitCode: 1, Err: exec.ErrNotFound}
	if !errors.Is(ce, exec.ErrNotFound) {
		t.Error("CommandError should unwrap to its cause")
	}
}

func TestExecTimeout(t *testing.T) {
	// A probe that hangs turns a diagnostic into another thing that needs
	// diagnosing, so the bound is part of the contract.
	s := New(Options{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := s.Exec(context.Background(), "sleep", "5")
	if err == nil {
		t.Fatal("expected the sleep to be cut short")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Exec took %v; the timeout was not applied", elapsed)
	}
}

func TestInspectMode(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name       string
		mode       fs.FileMode
		want       fs.FileMode
		wantOK     bool
		wantExists bool
	}{
		{"0600 file meets 0600", 0o600, 0o600, true, true},
		// Stricter is fine. What matters is that nothing is reachable by group
		// or other — a world-readable ~/.latem holding a machine token and
		// every secret is the whole threat model failing quietly (§8).
		{"0400 is stricter than 0600", 0o400, 0o600, true, true},
		{"0644 leaks to other", 0o644, 0o600, false, true},
		{"0660 leaks to group", 0o660, 0o600, false, true},
		{"0777 fails everything", 0o777, 0o700, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_"))
			if err := mustWrite(path, "x", tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			got := InspectMode(path, tt.want)
			if got.Exists != tt.wantExists {
				t.Fatalf("Exists = %v", got.Exists)
			}
			if got.OK != tt.wantOK {
				t.Errorf("InspectMode(%v, want %v).OK = %v, want %v", tt.mode, tt.want, got.OK, tt.wantOK)
			}
			if !got.OK && !strings.HasPrefix(got.FixCommand(), "chmod ") {
				t.Errorf("FixCommand = %q, should be a chmod an operator can paste", got.FixCommand())
			}
		})
	}
}

func TestInspectModeMissingPath(t *testing.T) {
	// Many of these paths appear only after the agent has run once, so absence
	// is an answer rather than an error.
	got := InspectMode(filepath.Join(t.TempDir(), "nope"), 0o600)
	if got.Exists || got.Err != nil {
		t.Errorf("InspectMode on an absent path = %+v, want Exists false and no error", got)
	}
	if got := InspectMode("", 0o600); got.Exists {
		t.Error("the empty path should report nothing")
	}
}

func TestInspectModeDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got := InspectMode(dir, 0o700)
	if !got.IsDir || !got.OK {
		t.Errorf("InspectMode on a 0700 dir = %+v", got)
	}
}

func TestHTTPProbeURL(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"wss becomes https", "wss://api.example.com/agent", "https://api.example.com/agent", false},
		{"ws becomes http", "ws://localhost:8080/agent", "http://localhost:8080/agent", false},
		{"https untouched", "https://api.example.com/", "https://api.example.com/", false},
		{"empty", "", "", true},
		{"wrong scheme", "ssh://api.example.com", "", true},
		{"unparsable", "://x", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := httpProbeURL(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("httpProbeURL(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("httpProbeURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestClockSkew(t *testing.T) {
	// A machine token is validated against a signing window, so a clock a few
	// minutes out makes every connection fail authentication and the operator
	// spends the afternoon regenerating a token that was never the problem.
	skewed := time.Now().Add(-10 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", skewed.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New(Options{HTTP: srv.Client(), Timeout: 5 * time.Second})
	got, err := s.ClockSkew(context.Background(), strings.Replace(srv.URL, "http://", "ws://", 1))
	if err != nil {
		t.Fatalf("ClockSkew: %v", err)
	}
	if !got.Measured {
		t.Fatal("Measured is false despite a Date header")
	}
	if got.Abs() < 9*time.Minute || got.Abs() > 11*time.Minute {
		t.Errorf("Delta = %v, want about 10m", got.Delta)
	}
	if got.Delta < 0 {
		t.Errorf("Delta = %v; the local clock is ahead here, so it should be positive", got.Delta)
	}
}

func TestClockSkewNoDateHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Date"] = nil
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New(Options{HTTP: srv.Client(), Timeout: 5 * time.Second})
	got, err := s.ClockSkew(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected an error when no Date header comes back")
	}
	if got.Measured {
		t.Error("Measured must stay false; a zero Delta would read as a perfect clock")
	}
}

func TestClockSkewBadURL(t *testing.T) {
	s := New(Options{Timeout: time.Second})
	if _, err := s.ClockSkew(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty broker URL")
	}
}

func TestSkewAbs(t *testing.T) {
	if got := (Skew{Delta: -3 * time.Second}).Abs(); got != 3*time.Second {
		t.Errorf("Abs = %v, want 3s", got)
	}
	if got := (Skew{Delta: 3 * time.Second}).Abs(); got != 3*time.Second {
		t.Errorf("Abs = %v, want 3s", got)
	}
}
