package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
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

// DockerSocket is where the agent service finds Docker: the client's default,
// because latem's unit sets no DOCKER_HOST.
const DockerSocket = "/var/run/docker.sock"

// ProcessCanConnect reports whether a running process's own credentials let it
// connect to the Unix socket at path, which takes write permission.
//
// [System.InDockerGroup] answers for the shell asking, and on Linux the agent
// is not in that shell. A systemd user service has the groups its user manager
// started with, and linger keeps that manager through a logout, so a group
// added since reaches every new login and never reaches the agent. Reading
// /proc is the one way to ask about the process that matters.
//
// An ACL on the socket is not read: it is not how Docker is set up, and the
// answer would be "no" for a socket that does let the process in.
func (s *System) ProcessCanConnect(pid int, path string) (bool, error) {
	info, err := os.Stat(s.sysPath(path))
	if err != nil {
		return false, err
	}
	owner, group, ok := fileOwner(info)
	if !ok {
		return false, fmt.Errorf("system: %s has no owner to compare", path)
	}
	data, err := os.ReadFile(s.sysPath(fmt.Sprintf("/proc/%d/status", pid)))
	if err != nil {
		return false, err
	}
	uid, gids, err := procCredentials(data)
	if err != nil {
		return false, fmt.Errorf("system: process %d: %w", pid, err)
	}
	mode := info.Mode().Perm()
	switch {
	case uid == 0:
		return true, nil
	case uid == owner:
		return mode&0o200 != 0, nil
	case slices.Contains(gids, group):
		return mode&0o020 != 0, nil
	default:
		return mode&0o002 != 0, nil
	}
}

// procCredentials reads the effective uid and every group from a
// /proc/<pid>/status:
//
//	Uid:	1000	1000	1000	1000
//	Gid:	1000	1000	1000	1000
//	Groups:	10 965 1000
//
// The second column of the first two is the effective id.
func procCredentials(status []byte) (uid uint32, gids []uint32, err error) {
	seenUID := false
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "Uid:", "Gid:":
			if len(fields) < 3 {
				return 0, nil, fmt.Errorf("short %s line", fields[0])
			}
			n, perr := strconv.ParseUint(fields[2], 10, 32)
			if perr != nil {
				return 0, nil, perr
			}
			if fields[0] == "Uid:" {
				uid, seenUID = uint32(n), true
			} else {
				gids = append(gids, uint32(n))
			}
		case "Groups:":
			for _, f := range fields[1:] {
				n, perr := strconv.ParseUint(f, 10, 32)
				if perr != nil {
					return 0, nil, perr
				}
				gids = append(gids, uint32(n))
			}
		}
	}
	if !seenUID {
		return 0, nil, errors.New("no Uid line")
	}
	return uid, gids, nil
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
