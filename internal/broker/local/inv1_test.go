//go:build unix

package local

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// INV-1 — The agent only makes outbound connections.
//
// This is the test RUNNER_SPEC §5 specifies verbatim: "start an agent,
// enumerate listening sockets for the process, assert every entry is a Unix
// socket or bound to 127.0.0.1". test/invariants/invariants_test.go holds
// TestINV1_NoInboundListeners as a skipped placeholder; that file is owned by
// another workstream, so the real assertion lives here, beside the only code
// in the tree that binds anything.
//
// # Why this enumerates file descriptors rather than shelling out
//
// `lsof -p` and `ss -ltnp` are the obvious tools and both are wrong for this.
// They are not present on every machine that runs the suite, their output is a
// moving target, and — decisively — parsing either one means the test passes
// when the tool is missing, which is a test that reports success for a machine
// it never looked at.
//
// Asking the kernel directly has none of those problems. SO_ACCEPTCONN says
// whether a descriptor is a listening socket; getsockname says what it is bound
// to. Both work identically on Linux and macOS, and a descriptor the process
// holds cannot hide from them.
//
// # What would break it
//
// Anything that gives the agent an inbound surface: an HTTP server for a web
// UI, a debug endpoint, a pprof listener someone left in, a metrics listener
// bound to 0.0.0.0 instead of 127.0.0.1. §20 permits the loopback metrics
// listener explicitly, which is why loopback is asserted rather than merely
// tolerated — the difference between 127.0.0.1 and 0.0.0.0 is one character
// and it turns onboarding into a router tutorial for every user.
func TestINV1_NoInboundListeners(t *testing.T) {
	// A live broker mid-conversation with a client, so the descriptors under
	// inspection include an accepted connection and not only the listener.
	b, db := newBroker(t)
	consume(t, b)

	c := newClient(t, b.Socket(), db)
	if _, err := c.Submit(t.Context(), runRequest("r_inv1"), SubmitOptions{}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	held := listeners(t)
	if len(held) == 0 {
		// The broker is up, so at least its own socket must appear. An empty
		// result would mean the enumeration is broken and the assertions below
		// pass against anything.
		t.Fatal("enumerated no listening sockets at all; the check is not looking at this process")
	}

	sawUnixSocket := false
	for _, l := range held {
		switch addr := l.addr.(type) {
		case *syscall.SockaddrUnix:
			sawUnixSocket = true
		case *syscall.SockaddrInet4:
			if ip := net.IP(addr.Addr[:]); !ip.IsLoopback() {
				t.Errorf("fd %d listens on %v:%d, which is not loopback (INV-1)", l.fd, ip, addr.Port)
			}
		case *syscall.SockaddrInet6:
			if ip := net.IP(addr.Addr[:]); !ip.IsLoopback() {
				t.Errorf("fd %d listens on [%v]:%d, which is not loopback (INV-1)", l.fd, ip, addr.Port)
			}
		default:
			t.Errorf("fd %d listens on an address of type %T; INV-1 permits Unix and loopback only", l.fd, addr)
		}
	}
	if !sawUnixSocket {
		t.Error("the broker's own Unix socket was not among the listening sockets; the enumeration missed it")
	}
}

// The negative control.
//
// A check that has only ever seen a compliant process cannot distinguish "no
// violations" from "no vision". This binds a non-loopback listener on purpose
// and asserts the enumeration reports it — without this, the test above would
// pass just as happily if listeners() returned nothing useful.
func TestINV1_DetectorSeesANonLoopbackListener(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot bind a non-loopback listener here: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Either family counts. Go binds "0.0.0.0:0" as a dual-stack [::] socket on
	// a machine with IPv6, so pinning this to SockaddrInet4 would make the
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

// Serving a command must not quietly acquire a port. The invariant is about
// the agent at rest and the agent at work, and only the second one has ever
// grown an HTTP server by accident.
func TestINV1_ServingACommandAddsNoIPListener(t *testing.T) {
	b, _ := newBroker(t)
	consume(t, b)

	before := countInet(listeners(t))

	conn := dial(t, b.Socket())
	if got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(runRequest("r_inv1_b")))); !got.OK {
		t.Fatalf("reply = %+v", got)
	}
	time.Sleep(50 * time.Millisecond)

	if after := countInet(listeners(t)); after != before {
		t.Errorf("serving a command changed the IP listener count from %d to %d", before, after)
	}
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
// with ENOPROTOOPT — for a real listening socket, not only for non-sockets.
// (Verified on Darwin 25: a net.Listen("tcp") descriptor returns "protocol not
// available".) A check written only against SO_ACCEPTCONN therefore enumerates
// nothing at all on exactly the platform this product's build machines run,
// and reports a clean INV-1 because it looked at zero descriptors. That is the
// failure mode the "enumerated no listening sockets at all" assertion above
// exists to catch, and it is why this function has two implementations rather
// than one.
//
// The fallback infers it: a stream socket that has a local address, has no
// peer, and is bound to a real port is one that something can connect to. It
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

func countInet(ls []listener) int {
	n := 0
	for _, l := range ls {
		switch l.addr.(type) {
		case *syscall.SockaddrInet4, *syscall.SockaddrInet6:
			n++
		}
	}
	return n
}
