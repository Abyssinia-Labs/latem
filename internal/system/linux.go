package system

import (
	"context"
	"errors"
	"os"
	"strings"
)

// DockerGroupName is the group Docker's socket is owned by on a conventional
// Linux install.
const DockerGroupName = "docker"

// Groups lists the groups the current user belongs to.
func (s *System) Groups(ctx context.Context) ([]string, error) {
	out, err := s.Exec(ctx, "id", "-nG")
	if err != nil {
		return nil, err
	}
	return strings.Fields(firstLine(out)), nil
}

// InDockerGroup reports whether this user can reach the Docker socket without
// sudo.
//
// It is a Linux question. On macOS, Docker Desktop's socket is owned by the
// logged-in user and there is no `docker` group to be in, so asking would
// produce a fault an operator cannot fix.
//
// The check is by group membership rather than by trying the socket because
// the two disagree in the case that matters: a user added to the group in a
// shell that has not been re-logged-in has the membership on file and not in
// the running session, and the fix — log out and back in — is one nobody
// guesses from "permission denied".
func (s *System) InDockerGroup(ctx context.Context) (bool, error) {
	if !s.IsLinux() {
		return true, nil
	}
	groups, err := s.Groups(ctx)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if g == DockerGroupName || g == "root" {
			return true, nil
		}
	}
	return false, nil
}

// Linger reports whether systemd keeps this user's services running after
// logout.
//
// Without it a systemd *user* unit stops the moment the operator's last
// session ends, which on a headless build machine is immediately after the
// install (RUNNER_SPEC §23). The service looks installed, `systemctl --user
// status` looks healthy while the operator is logged in, and the machine goes
// silent the moment they disconnect.
func (s *System) Linger(ctx context.Context, username string) (bool, error) {
	if !s.IsLinux() {
		return false, nil
	}
	if username == "" {
		username = s.username
	}
	out, err := s.Exec(ctx, "loginctl", "show-user", username, "--property=Linger")
	if err != nil {
		return false, err
	}
	return strings.Contains(strings.ToLower(string(out)), "linger=yes"), nil
}

// EnableLingerArgv is the command that turns linger on.
//
// It is returned rather than run so `latem doctor --fix` can print it and
// `latem service install` can execute the identical thing. A remediation an
// operator is shown and a remediation the tool performs drifting apart is how
// documentation stops being true.
func (s *System) EnableLingerArgv(username string) []string {
	if username == "" {
		username = s.username
	}
	return []string{"loginctl", "enable-linger", username}
}

// EnableLinger runs `loginctl enable-linger`.
func (s *System) EnableLinger(ctx context.Context, username string) error {
	argv := s.EnableLingerArgv(username)
	_, err := s.Exec(ctx, argv[0], argv[1:]...)
	return err
}

// KVM is the state of /dev/kvm.
type KVM struct {
	// Present reports whether the device node exists at all. Absent means
	// either no hardware virtualisation or a kernel module not loaded.
	Present bool
	// Usable reports whether this user can open it read-write, which is what
	// an Android emulator does (RUNNER_SPEC §18.2). Read-only access passes a
	// naive check and still fails the emulator.
	Usable bool
	Err    error
}

// CheckKVM inspects /dev/kvm.
//
// Android emulator jobs need it, and whether one is configured cannot be known
// from local config: `runs_on` and the job's steps live in jobs.yaml at the
// resolved commit, which is not on disk until a run checks it out (INV-5).
// So this reports the fact and leaves the severity to the caller.
func CheckKVM() KVM {
	const dev = "/dev/kvm"
	if _, err := os.Stat(dev); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return KVM{}
		}
		return KVM{Err: err}
	}
	k := KVM{Present: true}
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return k
	}
	_ = f.Close()
	k.Usable = true
	return k
}
