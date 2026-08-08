package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckLoopback(t *testing.T) {
	// INV-1 is the invariant the whole threat model rests on: the agent makes
	// outbound connections and binds nothing routable. This table is the
	// second line of that defence — internal/config rejects a bad value at
	// load, and this rejects one again at the moment a socket would be created.
	tests := []struct {
		name    string
		addr    string
		wantErr bool
		notLoop bool
	}{
		{name: "IPv4 loopback", addr: "127.0.0.1:9838"},
		{name: "IPv4 loopback, another address in the /8", addr: "127.0.0.2:9838"},
		{name: "IPv6 loopback", addr: "[::1]:9838"},
		{name: "localhost", addr: "localhost:9838"},
		{name: "port zero is fine", addr: "127.0.0.1:0"},

		{name: "routable IPv4", addr: "192.168.1.10:9838", wantErr: true, notLoop: true},
		{name: "all interfaces", addr: "0.0.0.0:9838", wantErr: true, notLoop: true},
		{name: "all interfaces, IPv6", addr: "[::]:9838", wantErr: true, notLoop: true},
		// ":9838" is the idiom people reach for and it binds every interface,
		// which is precisely what INV-1 forbids. It must be refused rather
		// than read as a default.
		{name: "empty host", addr: ":9838", wantErr: true, notLoop: true},
		{name: "public IP", addr: "8.8.8.8:9838", wantErr: true, notLoop: true},

		{name: "no port", addr: "127.0.0.1", wantErr: true},
		{name: "nonsense", addr: "not an address", wantErr: true},
		{name: "unresolvable host", addr: "no-such-host.invalid:9838", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckLoopback(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckLoopback(%q) = %v, wantErr %v", tt.addr, err, tt.wantErr)
			}
			if tt.notLoop && !errors.Is(err, ErrNotLoopback) {
				t.Errorf("CheckLoopback(%q) should report ErrNotLoopback, got %v", tt.addr, err)
			}
		})
	}
}

func TestServeRefusesNonLoopback(t *testing.T) {
	m := New("v1")
	s, err := Serve("0.0.0.0:0", m.Handler())
	if !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("Serve on a wildcard address = %v, want ErrNotLoopback", err)
	}
	if s != nil {
		t.Error("Serve returned a server despite refusing the address")
	}

	// Nothing may be listening after a refusal.
	if ln, lerr := net.Listen("tcp", "0.0.0.0:0"); lerr == nil {
		_ = ln.Close()
	}
}

func TestServeEmptyAddrDisables(t *testing.T) {
	// §8: metrics_addr = "" disables metrics, and that is a configuration, not
	// a failure. A nil Server must be usable in the shutdown path so an agent
	// does not have to branch on whether metrics were on.
	s, err := Serve("", New("v1").Handler())
	if err != nil {
		t.Fatalf("Serve(\"\") = %v, want no error", err)
	}
	if s != nil {
		t.Fatal("Serve(\"\") should return a nil server")
	}
	if got := s.Addr(); got != "" {
		t.Errorf("nil Server Addr = %q", got)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("closing a nil Server = %v", err)
	}
}

func TestServeRoundTrip(t *testing.T) {
	m := New("v1")
	m.RunsTotal.Inc("succeeded")

	s, err := Serve("127.0.0.1:0", m.Handler())
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	host, _, err := net.SplitHostPort(s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("bound to %s, which is not loopback", s.Addr())
	}

	resp, err := http.Get("http://" + s.Addr() + PathMetrics) //nolint:noctx // a loopback scrape in a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `latem_runs_total{state="succeeded"} 1`) {
		t.Errorf("scrape did not carry the counter:\n%s", body)
	}
}

func TestServePortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	if _, err := Serve(ln.Addr().String(), New("v1").Handler()); err == nil {
		t.Error("expected an error binding an address already in use")
	}
}

func TestDefaultAddrIsLoopback(t *testing.T) {
	if err := CheckLoopback(DefaultAddr); err != nil {
		t.Errorf("the documented default %q fails its own check: %v", DefaultAddr, err)
	}
}
