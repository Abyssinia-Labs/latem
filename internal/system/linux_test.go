package system

import (
	"context"
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
