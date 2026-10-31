package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInDockerGroup(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		cmds    map[string]fakeCmd
		want    bool
		wantErr bool
	}{
		{"member", "linux", map[string]fakeCmd{"id -nG": {out: "tester sudo docker\n"}}, true, false},
		{
			// The fix — log out and back in — is one nobody guesses from
			// "permission denied while trying to connect to the Docker daemon
			// socket", which is why this is a check rather than a runtime
			// error message.
			name: "not a member",
			goos: "linux",
			cmds: map[string]fakeCmd{"id -nG": {out: "tester sudo\n"}},
		},
		{"root needs no group", "linux", map[string]fakeCmd{"id -nG": {out: "root\n"}}, true, false},
		{
			// Docker Desktop's socket is owned by the logged-in user and there
			// is no `docker` group to be in, so asking on macOS would produce
			// a fault an operator cannot fix.
			name: "macOS has no docker group",
			goos: "darwin",
			want: true,
		},
		{"id unavailable", "linux", nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem(tt.goos, newFakeRunner(tt.cmds), nil, "")
			got, err := s.InDockerGroup(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("InDockerGroup = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGroups(t *testing.T) {
	s := testSystem("linux", newFakeRunner(map[string]fakeCmd{"id -nG": {out: "tester docker kvm\n"}}), nil, "")
	got, err := s.Groups(context.Background())
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(got) != 3 || got[1] != "docker" {
		t.Errorf("Groups = %v, want [tester docker kvm]", got)
	}
}

func TestLinger(t *testing.T) {
	tests := []struct {
		name string
		goos string
		cmds map[string]fakeCmd
		want bool
	}{
		{
			name: "enabled",
			goos: "linux",
			cmds: map[string]fakeCmd{"loginctl show-user tester --property=Linger": {out: "Linger=yes\n"}},
			want: true,
		},
		{
			// Without it the unit stops the moment the operator's last session
			// ends, which on a headless build machine is right after install.
			name: "disabled",
			goos: "linux",
			cmds: map[string]fakeCmd{"loginctl show-user tester --property=Linger": {out: "Linger=no\n"}},
		},
		{"macOS has no loginctl", "darwin", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSystem(tt.goos, newFakeRunner(tt.cmds), nil, "")
			got, _ := s.Linger(context.Background(), "")
			if got != tt.want {
				t.Errorf("Linger = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnableLinger(t *testing.T) {
	s := testSystem("linux", newFakeRunner(map[string]fakeCmd{"loginctl enable-linger tester": {}}), nil, "")

	// The argv the operator is shown and the argv the tool runs must be the
	// same list, or the runbook stops being true.
	argv := s.EnableLingerArgv("")
	if strings.Join(argv, " ") != "loginctl enable-linger tester" {
		t.Fatalf("EnableLingerArgv = %v", argv)
	}
	if err := s.EnableLinger(context.Background(), ""); err != nil {
		t.Errorf("EnableLinger: %v", err)
	}
	if got := s.EnableLingerArgv("other"); got[2] != "other" {
		t.Errorf("EnableLingerArgv(other) = %v", got)
	}
}

func TestCheckKVM(t *testing.T) {
	// /dev/kvm is a real device or it is not; the test asserts the shape of
	// the answer rather than the machine's hardware, because CI runs on both.
	got := CheckKVM()
	if !got.Present && got.Usable {
		t.Error("an absent /dev/kvm cannot be usable")
	}
	if got.Err != nil && got.Present {
		t.Errorf("Present with an error set: %+v", got)
	}
}

// With no root, which is every real machine, a system path stays absolute. A
// relative one resolves from `/` and from nowhere else, so memory read as zero
// for any process started in a home directory, and every test passed because
// every test sets a root.
func TestSysPathIsAbsoluteOnARealMachine(t *testing.T) {
	s := New(Options{GOOS: "linux", Home: t.TempDir(), Getenv: func(string) string { return "" }})
	for _, p := range []string{"proc/meminfo", "/proc/meminfo"} {
		if got := s.sysPath(p); got != "/proc/meminfo" {
			t.Errorf("sysPath(%q) = %q, want /proc/meminfo", p, got)
		}
	}
	rooted := New(Options{GOOS: "linux", Root: "/fixture", Home: t.TempDir(), Getenv: func(string) string { return "" }})
	if got := rooted.sysPath("proc/meminfo"); got != "/fixture/proc/meminfo" {
		t.Errorf("rooted = %q", got)
	}
}

// The agent is a child of its user manager, so what counts is what the process
// holds. Membership on file, and the shell asking, are both beside the point.
func TestProcessCanConnect_GoesByWhatTheProcessHolds(t *testing.T) {
	root := t.TempDir()
	sock := filepath.Join(root, DockerSocket)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o660); err != nil {
		t.Fatal(err)
	}
	// The fixture's own owner and group, whatever they are where this runs.
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	owner, group, ok := fileOwner(info)
	if !ok {
		t.Fatal("the fixture has no owner")
	}
	stranger, other := owner+1, group+1

	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"holds the socket's group", fmt.Sprintf("Uid:\t%[1]d\t%[1]d\t%[1]d\t%[1]d\nGid:\t%[2]d\t%[2]d\t%[2]d\t%[2]d\nGroups:\t%[2]d %[3]d \n", stranger, other, group), true},
		{"has it as its primary group", fmt.Sprintf("Uid:\t%[1]d\t%[1]d\t%[1]d\t%[1]d\nGid:\t%[2]d\t%[2]d\t%[2]d\t%[2]d\nGroups:\t\n", stranger, group), true},
		{"started before the group was added", fmt.Sprintf("Uid:\t%[1]d\t%[1]d\t%[1]d\t%[1]d\nGid:\t%[2]d\t%[2]d\t%[2]d\t%[2]d\nGroups:\t%[2]d \n", stranger, other), false},
		{"owns the socket", fmt.Sprintf("Uid:\t%[1]d\t%[1]d\t%[1]d\t%[1]d\nGid:\t%[2]d\t%[2]d\t%[2]d\t%[2]d\nGroups:\t\n", owner, other), true},
		{"root", fmt.Sprintf("Uid:\t0\t0\t0\t0\nGid:\t%[1]d\t%[1]d\t%[1]d\t%[1]d\nGroups:\t\n", other), true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid := 100 + i
			dir := filepath.Join(root, "proc", fmt.Sprint(pid))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tlatem\n"+tt.status), 0o644); err != nil {
				t.Fatal(err)
			}
			s := New(Options{GOOS: "linux", Root: root, Home: t.TempDir(), Getenv: func(string) string { return "" }})
			got, err := s.ProcessCanConnect(pid, DockerSocket)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("ProcessCanConnect = %v, want %v", got, tt.want)
			}
		})
	}

	s := New(Options{GOOS: "linux", Root: root, Home: t.TempDir(), Getenv: func(string) string { return "" }})
	if _, err := s.ProcessCanConnect(999, DockerSocket); err == nil {
		t.Error("a process that has gone gave an answer")
	}
	if _, err := s.ProcessCanConnect(100, "/var/run/elsewhere.sock"); err == nil {
		t.Error("a socket that is not there gave an answer")
	}
}
