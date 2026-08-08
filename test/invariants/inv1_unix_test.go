//go:build unix

package invariants

// INV-1 lives in its own file because its detector cannot be written portably.
//
// Enumerating a process's own listening sockets means asking the kernel about
// file descriptors, and the calls that do it — Getsockname, GetsockoptInt,
// Getrlimit — take an int fd on Unix and do not exist at all on Windows. A
// package whose only file was tagged would have no files on Windows and fail
// to build; splitting it here keeps INV-2 through INV-5 portable and confines
// the constraint to the one test that earns it. Every platform RUNNER_SPEC
// targets is Unix anyway — the agent's own lock needs flock.
//
// The alternative, a "portable" detector that shells out to `lsof` or `ss`,
// was rejected for the reason internal/broker/local's copy gives: it passes
// when the tool is missing, which is a test that reports success for a machine
// it never looked at.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
)

// TestINV1_NoInboundListeners — the agent only makes outbound connections.
//
// Start an agent, enumerate listening sockets for the process, assert every
// entry is a Unix socket or bound to loopback. There is no HTTP server, no
// port to forward, and no inbound firewall rule; getting this wrong turns
// onboarding into a router tutorial for every user.
//
// The loopback metrics listener (RUNNER_SPEC §20) is permitted and is asserted
// as loopback rather than merely tolerated — the difference between 127.0.0.1
// and 0.0.0.0 is one character.
//
// # The agent is real, and it is in this process
//
// The agent runs in-process so its descriptors are this process's descriptors.
// A subprocess would be more faithful to `latem agent` and would put every
// socket it opens out of reach of any detector that does not parse another
// process's kernel state — which is exactly the class of tool this test exists
// to avoid depending on.
func TestINV1_NoInboundListeners(t *testing.T) {
	a := startAgentForINV1(t)

	held := listeners(t)
	if len(held) == 0 {
		// The agent is up, so at least its own Unix socket must appear. An
		// empty result means the enumeration is broken and every assertion
		// below would pass against anything — which is precisely the failure
		// amendment A25 records.
		t.Fatal("enumerated no listening sockets at all; the check is not looking at this process")
	}

	var sawUnix, sawMetrics bool
	for _, l := range held {
		switch addr := l.addr.(type) {
		case *syscall.SockaddrUnix:
			sawUnix = true
		case *syscall.SockaddrInet4:
			ip := net.IP(addr.Addr[:])
			if !ip.IsLoopback() {
				t.Errorf("fd %d listens on %v:%d, which is not loopback (INV-1)", l.fd, ip, addr.Port)
				continue
			}
			if portOf(a.MetricsAddr()) == addr.Port {
				sawMetrics = true
			}
		case *syscall.SockaddrInet6:
			ip := net.IP(addr.Addr[:])
			if !ip.IsLoopback() {
				t.Errorf("fd %d listens on [%v]:%d, which is not loopback (INV-1)", l.fd, ip, addr.Port)
				continue
			}
			if portOf(a.MetricsAddr()) == addr.Port {
				sawMetrics = true
			}
		default:
			t.Errorf("fd %d listens on an address of type %T; INV-1 permits Unix and loopback only", l.fd, addr)
		}
	}

	if !sawUnix {
		t.Error("the agent's own Unix socket was not among the listening sockets; the enumeration missed it")
	}
	if a.MetricsAddr() != "" && !sawMetrics {
		t.Errorf("the loopback metrics listener on %s was not enumerated", a.MetricsAddr())
	}
}

// TestINV1_DetectorSeesANonLoopbackListener is the negative control.
//
// A check that has only ever seen a compliant process cannot distinguish "no
// violations" from "no vision". Amendment A25 records exactly how that goes
// wrong here: getsockopt(SO_ACCEPTCONN) returns ENOPROTOOPT on Darwin for a
// genuine listening socket, so the obvious detector enumerates zero
// descriptors and reports the invariant clean having examined nothing. This
// binds 0.0.0.0 on purpose and requires the detector to object.
func TestINV1_DetectorSeesANonLoopbackListener(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot bind a non-loopback listener here: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Either family counts. Go binds "0.0.0.0:0" as a dual-stack [::] socket
	// on a machine with IPv6, so pinning this to SockaddrInet4 would make the
	// control itself platform-dependent.
	for _, l := range listeners(t) {
		var ip net.IP
		switch a := l.addr.(type) {
		case *syscall.SockaddrInet4:
			ip = net.IP(a.Addr[:])
		case *syscall.SockaddrInet6:
			ip = net.IP(a.Addr[:])
		default:
			continue
		}
		if ip.IsUnspecified() {
			return // detected, as required
		}
	}
	t.Fatal("a listener on 0.0.0.0 was not detected; TestINV1_NoInboundListeners cannot fail")
}

// startAgentForINV1 brings up a real agent with a loopback metrics endpoint.
func startAgentForINV1(t *testing.T) *agent.Agent {
	t.Helper()

	// Short, because a Unix socket path is capped at 103 bytes by the kernel
	// and the default temporary directory on macOS is longer than that before
	// anything is joined to it. config.Load rejects the overrun with a clear
	// message, so a test using t.TempDir() would fail on its own scratch space
	// rather than on the invariant.
	home, err := os.MkdirTemp("/tmp", "inv1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, ".latem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`schema_version = 1

[machine]
name = "inv1"
id   = "mch_inv1"

[agent]
metrics_addr = %q

[broker.local]
enabled = true

[repos.app]
url          = "https://git.example.invalid/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "inv1"
executor    = "native"
labels      = ["inv1"]
concurrency = 1

  [runner.native]
  min_free_gb = 0.001
`, freeLoopbackAddr(t))

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.Options{Dir: dir, Home: home, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("loading the config this test just wrote: %v", err)
	}

	a, err := agent.New(agent.Options{Config: cfg, Version: "inv1"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(60 * time.Second):
			t.Error("the agent did not shut down")
		}
	})

	select {
	case <-a.Ready():
	case <-stopped:
		t.Fatal("the agent exited during startup")
	case <-time.After(60 * time.Second):
		t.Fatal("the agent never became ready")
	}
	return a
}

// freeLoopbackAddr picks a loopback port that was free a moment ago.
//
// The config loader rejects port 0 — INV-1 is enforced in the parser and a
// port must be 1..65535 — so the address cannot be left for the kernel to
// choose at bind time. The race is theoretical on a test machine, and the
// alternative is a hard-coded 9838 that collides with the developer's own
// agent.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// portOf extracts the port from a host:port address, or -1.
func portOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		return -1
	}
	return n
}

type listener struct {
	fd   int
	addr syscall.Sockaddr
}

// listeners returns every listening socket this process holds.
//
// The descriptor space is walked rather than read from /proc, because /proc is
// Linux-only and this suite runs on the macOS build machines the product is
// mostly for. EBADF and ENOTSOCK are the ordinary answers across most of the
// range and are skipped in silence.
//
// This is the detector from internal/broker/local/inv1_test.go, deliberately
// duplicated rather than shared: it is a test file in another package, and the
// alternative — exporting a socket-enumerating helper from a production
// package — would put a descriptor walker in the shipped binary to satisfy a
// test.
func listeners(t *testing.T) []listener {
	t.Helper()

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	// A soft limit of "unlimited" is common on macOS. The test process holds a
	// few dozen descriptors, so a bounded walk finds all of them without
	// spending a second on syscalls that return EBADF.
	ceiling := int(lim.Cur)
	if ceiling <= 0 || ceiling > 4096 {
		ceiling = 4096
	}

	var out []listener
	for fd := range ceiling {
		if addr, ok := listening(fd); ok {
			out = append(out, listener{fd: fd, addr: addr})
		}
	}
	return out
}

// listening reports whether fd is a socket that accepts connections, and what
// it is bound to.
//
// # macOS does not answer the obvious question
//
// SO_ACCEPTCONN is the direct way to ask, and on Darwin getsockopt refuses it
// with ENOPROTOOPT — for a real listening socket, not only for a non-socket
// (amendment A25, verified on Darwin 25). A check written only against
// SO_ACCEPTCONN therefore enumerates nothing at all on exactly the platform
// this product's build machines run, and reports a clean INV-1 because it
// looked at zero descriptors.
//
// The fallback infers it: a stream socket that has a local address, has no
// peer, and is bound to a real port is one something can connect to. It
// over-reports rather than under-reports — a socket mid-connect could look
// like a listener — which is the safe direction for an invariant test.
func listening(fd int) (syscall.Sockaddr, bool) {
	addr, err := syscall.Getsockname(fd)
	if err != nil {
		return nil, false // not a socket, or not open
	}

	if v, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN); err == nil {
		return addr, v != 0
	}

	if typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || typ != syscall.SOCK_STREAM {
		return nil, false
	}
	if _, err := syscall.Getpeername(fd); !errors.Is(err, syscall.ENOTCONN) {
		return nil, false // connected, so it is somebody's accepted or dialled socket
	}
	// Port zero means bound to nothing yet, which is a socket on its way
	// somewhere rather than one waiting to be reached.
	switch a := addr.(type) {
	case *syscall.SockaddrInet4:
		return addr, a.Port != 0
	case *syscall.SockaddrInet6:
		return addr, a.Port != 0
	default:
		return addr, true
	}
}
