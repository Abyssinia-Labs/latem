package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultAddr is RUNNER_SPEC §20's default: loopback, port 9838.
const DefaultAddr = "127.0.0.1:9838"

// ErrNotLoopback is returned for an address that is not on loopback.
//
// It is a distinct error because the caller's response is specific: refuse to
// start, and say which invariant was violated. INV-1 is not a preference that
// a flag can override.
var ErrNotLoopback = errors.New("metrics: address is not loopback (INV-1)")

// CheckLoopback reports whether addr is safe to bind under INV-1.
//
// The rule: every address the host resolves to must be loopback. Not "the
// first one" — a name resolving to both 127.0.0.1 and a routable address would
// pass a first-match check and then bind the routable one, because which
// address a listener picks is the resolver's business and not the caller's.
//
// An empty host — ":9838" — is a wildcard bind on every interface and is
// exactly what INV-1 forbids, so it is refused rather than treated as a
// default.
func CheckLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("metrics: %q is not a host:port address: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("metrics: %q has no port", addr)
	}
	if host == "" {
		return fmt.Errorf("%w: %q binds every interface; use 127.0.0.1:%s", ErrNotLoopback, addr, port)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("%w: %s", ErrNotLoopback, addr)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("metrics: cannot resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("metrics: %q resolves to nothing", host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("%w: %s resolves to %s", ErrNotLoopback, host, ip)
		}
	}
	return nil
}

// Server is a running metrics endpoint.
type Server struct {
	srv *http.Server
	ln  net.Listener
	err chan error
}

// Serve binds addr and serves the metrics endpoints.
//
// The address is validated before the socket is created and the bound address
// is validated again after. The second check is not paranoia about the first:
// a hostname can resolve to a loopback address at check time and a listener
// can still end up somewhere else if the resolution changed underneath, and
// the cost of being wrong is an inbound surface on a machine that holds
// signing certificates.
//
// An empty addr disables metrics and returns a nil Server, which is the
// documented meaning of `metrics_addr = ""` in §8 and not an error.
func Serve(addr string, h http.Handler) (*Server, error) {
	if addr == "" {
		return nil, nil
	}
	if err := CheckLoopback(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: listening on %s: %w", addr, err)
	}
	if err := CheckLoopback(ln.Addr().String()); err != nil {
		_ = ln.Close()
		return nil, err
	}

	s := &Server{
		srv: &http.Server{
			Handler: h,
			// A scrape is a few kilobytes of text from a client on the same
			// machine. The timeouts exist so a stuck client cannot pin a
			// goroutine and a file descriptor for the life of the agent.
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		ln:  ln,
		err: make(chan error, 1),
	}
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.err <- err
	}()
	return s, nil
}

// Addr is where the server actually bound, which differs from the requested
// address when the port was 0.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close shuts the server down, waiting for in-flight scrapes.
//
// Calling it on a nil Server is fine: `metrics_addr = ""` produces one, and an
// agent's shutdown path should not have to branch on whether metrics were
// enabled.
func (s *Server) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := s.srv.Shutdown(ctx); err != nil {
		return err
	}
	return <-s.err
}
