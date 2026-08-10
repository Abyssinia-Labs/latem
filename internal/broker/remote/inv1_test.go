//go:build unix

package remote

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// INV-1 — The agent only makes outbound connections.
//
// internal/broker/local asserts the general form of this: every listening
// socket the process holds is a Unix socket or bound to loopback. This file
// asserts the sharper claim that belongs to the remote transport, and it is the
// claim the product's installability rests on: connecting to a control plane
// adds **no** listening socket at all. Not a loopback one, not a high port
// nobody will notice, none.
//
// That distinction matters because the loopback rule has an escape hatch in it
// — §20's metrics endpoint is a legitimate loopback listener — and a WebSocket
// library that decided to open a local port for some internal purpose would
// slip through a test written only in those terms. The counting test below
// cannot be satisfied that way.
//
// The helpers are a second copy of internal/broker/local's, including the
// Darwin caveat in amendment A25, because a test package cannot import another
// package's tests. The duplication is recorded rather than worked around.

// TestINV1_RemoteBrokerBindsNothing counts listening sockets across a whole
// session — dial, resume, report, log, reconnect — and requires the count to be
// unchanged at the end.
func TestINV1_RemoteBrokerBindsNothing(t *testing.T) {
	const runID = "r_inv1"

	// The control plane's own httptest listener is up before the count is taken,
	// so it is part of the baseline rather than part of the result.
	h := newHarness(t, nil)
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)

	before := listeners(t)
	if len(before) == 0 {
		// The test's own control plane is listening, so an empty enumeration
		// means the check is broken and every assertion below is vacuous — the
		// exact failure amendment A25 records for macOS.
		t.Fatal("enumerated no listening sockets at all; the check is not looking at this process")
	}

	// Everything the transport does, while it is being watched.
	if err := h.Heartbeat(t.Context(), protocol.MachineHealth{MachineID: "mch_test"}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := h.Report(t.Context(), runID, protocol.Update{State: protocol.StateCollecting}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	sink, err := h.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	writeFrames(t, sink, runID, 1, 20, "inv1")
	h.CP.Await(t, "the frames to be acknowledged", func() bool { return sink.Acked() >= 20 })

	// And a reconnect, because the dial path is the one that would grow a
	// listener if anything did.
	h.CP.Partition()
	h.CP.Await(t, "the partition", func() bool { return !h.Connected() })
	h.CP.Heal()
	h.CP.Await(t, "the reconnect", func() bool { return h.Connected() })
	h.AwaitResumes(t, 2)

	after := listeners(t)
	if len(after) != len(before) {
		for _, l := range after {
			t.Logf("listening fd %d on %s", l.fd, describeAddr(l.addr))
		}
		t.Fatalf("the remote broker changed the process's listening-socket count from %d to %d; INV-1 says the agent binds nothing",
			len(before), len(after))
	}

	// The general rule, asserted as well, because a listener that replaced
	// another would keep the count the same.
	for _, l := range after {
		switch addr := l.addr.(type) {
		case *syscall.SockaddrUnix:
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
}

// The negative control.
//
// A check that has only ever seen a compliant process cannot distinguish "no
// violations" from "no vision". This binds a non-loopback listener on purpose
// and requires the enumeration to see it and the count to move.
func TestINV1_DetectorSeesANonLoopbackListener(t *testing.T) {
	before := len(listeners(t))

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot bind a non-loopback listener here: %v", err)
	}
	defer func() { _ = ln.Close() }()

	after := listeners(t)
	if len(after) != before+1 {
		t.Fatalf("binding a listener moved the count from %d to %d; the counting assertion above cannot fail",
			before, len(after))
	}

	// Either family counts: Go binds "0.0.0.0:0" as a dual-stack [::] socket on
	// a machine with IPv6, so pinning this to SockaddrInet4 would make the
	// control itself platform-dependent.
	for _, l := range after {
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
	t.Fatal("a listener on 0.0.0.0 was not classified as non-loopback; TestINV1_RemoteBrokerBindsNothing cannot fail")
}

type listener struct {
	fd   int
	addr syscall.Sockaddr
}

// listeners returns every listening socket this process holds.
//
// The descriptor space is walked rather than read from /proc, because /proc is
// Linux-only and this suite runs on the macOS build machines the product is
// mostly for.
func listeners(t *testing.T) []listener {
	t.Helper()

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
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
// SO_ACCEPTCONN is the direct way to ask, and on Darwin getsockopt refuses it
// with ENOPROTOOPT for a real listening socket (amendment A25). A check written
// only against it enumerates nothing on exactly the platform this product's
// build machines run. The fallback infers it: a stream socket with a local
// address, no peer, and a non-zero port is one something can connect to.
func listening(fd int) (syscall.Sockaddr, bool) {
	addr, err := syscall.Getsockname(fd)
	if err != nil {
		return nil, false
	}
	if v, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN); err == nil {
		return addr, v != 0
	}
	if typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || typ != syscall.SOCK_STREAM {
		return nil, false
	}
	if _, err := syscall.Getpeername(fd); !errors.Is(err, syscall.ENOTCONN) {
		return nil, false
	}
	switch a := addr.(type) {
	case *syscall.SockaddrInet4:
		return addr, a.Port != 0
	case *syscall.SockaddrInet6:
		return addr, a.Port != 0
	default:
		return addr, true
	}
}

func describeAddr(a syscall.Sockaddr) string {
	switch v := a.(type) {
	case *syscall.SockaddrUnix:
		return "unix " + v.Name
	case *syscall.SockaddrInet4:
		return net.JoinHostPort(net.IP(v.Addr[:]).String(), itoa(uint64(v.Port)))
	case *syscall.SockaddrInet6:
		return net.JoinHostPort(net.IP(v.Addr[:]).String(), itoa(uint64(v.Port)))
	default:
		return "unknown"
	}
}
