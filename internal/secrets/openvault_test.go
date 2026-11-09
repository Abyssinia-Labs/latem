package secrets

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ovRequest is one request the fake service received, decoded.
type ovRequest struct {
	typ    byte
	fields []string
}

// fakeVault is an OpenVault service on a real Unix socket, speaking
// service.md's framing, so the provider is tested over the bytes the real one
// reads rather than through a seam that would agree with whatever it sent.
type fakeVault struct {
	t      *testing.T
	ln     *net.UnixListener
	socket string

	mu       sync.Mutex
	requests []ovRequest
	// answer is the response frame's type and body for a request.
	answer func(r ovRequest) []byte
}

// ovSocketPath is a socket path short enough for sun_path. t.TempDir embeds
// the test's name and passes 104 bytes on macOS; see broker/local's
// socketPath for the failure that looks like.
func ovSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ov")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, OpenVaultSocketName)
	if len(path) > 103 {
		t.Fatalf("socket path %s is %d bytes; the kernel caps sun_path at 104", path, len(path))
	}
	return path
}

func newFakeVault(t *testing.T, answer func(r ovRequest) []byte) *fakeVault {
	t.Helper()
	socket := ovSocketPath(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVault{t: t, ln: ln, socket: socket, answer: answer}
	t.Cleanup(func() { _ = ln.Close() })
	go v.serve()
	return v
}

func (v *fakeVault) serve() {
	for {
		conn, err := v.ln.Accept()
		if err != nil {
			return
		}
		go v.handle(conn)
	}
}

func (v *fakeVault) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return
	}
	buf := make([]byte, binary.BigEndian.Uint32(head[:]))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	req := ovRequest{typ: buf[0]}
	for rest := buf[1:]; len(rest) >= 2; {
		n := int(binary.BigEndian.Uint16(rest))
		req.fields = append(req.fields, string(rest[2:2+n]))
		rest = rest[2+n:]
	}
	v.mu.Lock()
	v.requests = append(v.requests, req)
	v.mu.Unlock()

	resp := v.answer(req)
	if resp == nil {
		return
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(len(resp)))
	_, _ = conn.Write(append(out, resp...))
}

func (v *fakeVault) got() []ovRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.requests)
}

func ovFound(kind byte, value string) []byte {
	b := []byte{ovEntry, 1, kind}
	b = binary.BigEndian.AppendUint32(b, uint32(len(value)))
	return append(b, value...)
}

func ovNotFound() []byte { return []byte{ovEntry, 0} }

func ovNameList(names ...string) []byte {
	b := binary.BigEndian.AppendUint32([]byte{ovNames}, uint32(len(names)))
	for _, n := range names {
		b = append(b, ovStr(n)...)
	}
	return b
}

func ovListingOf(entries map[string]byte) []byte {
	b := binary.BigEndian.AppendUint32([]byte{ovListing}, uint32(len(entries)))
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range entries {
			if !yield(k) {
				return
			}
		}
	}) {
		b = append(append(b, ovStr(name)...), entries[name])
	}
	return b
}

func ovRefusal(msg string) []byte { return append([]byte{ovError, 0x01}, ovStr(msg)...) }

// vaultOf answers get from values, keyed collection + "/" + entry, and
// collections with the collections the keys name.
func vaultOf(values map[string]string) func(r ovRequest) []byte {
	return func(r ovRequest) []byte {
		switch r.typ {
		case ovGet:
			collection := r.fields[0]
			has := false
			for k := range values {
				has = has || strings.HasPrefix(k, collection+"/")
			}
			if !has {
				return ovRefusal("no collection " + collection)
			}
			if v, ok := values[collection+"/"+r.fields[1]]; ok {
				return ovFound(ovSecret, v)
			}
			return ovNotFound()
		case ovCollections:
			var names []string
			for k := range values {
				c, _, _ := strings.Cut(k, "/")
				if !slices.Contains(names, c) {
					names = append(names, c)
				}
			}
			slices.Sort(names)
			return ovNameList(names...)
		}
		return ovRefusal("unexpected request")
	}
}

func TestOpenVault_AnEnvironmentsSecretIsTheRepositorysCollectionUnderTheEnvironmentsFolder(t *testing.T) {
	v := newFakeVault(t, vaultOf(map[string]string{"latem-cloud/production/CONVEX_DEPLOY_KEY": "prod-key-0123456789"}))
	p := NewOpenVaultProvider(v.socket, 0)

	got, err := p.ResolveIn(context.Background(), []string{"latem-cloud", "production"}, "CONVEX_DEPLOY_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "prod-key-0123456789" {
		t.Errorf("value = %q", got)
	}
	// The collection is the repository key as written, hyphen and all: the
	// flattened latem_cloud names no collection.
	want := []ovRequest{{typ: ovGet, fields: []string{"latem-cloud", "production/CONVEX_DEPLOY_KEY"}}}
	if reqs := v.got(); !slices.EqualFunc(reqs, want, ovRequestEqual) {
		t.Errorf("requests = %+v, want %+v", reqs, want)
	}
}

func TestOpenVault_ARepositorysSecretIsAtTheTopOfItsCollection(t *testing.T) {
	v := newFakeVault(t, vaultOf(map[string]string{"app/NPM_TOKEN": "npm-0123456789abcdef"}))
	p := NewOpenVaultProvider(v.socket, 0)

	got, err := p.ResolveIn(context.Background(), []string{"app"}, "NPM_TOKEN")
	if err != nil || string(got) != "npm-0123456789abcdef" {
		t.Fatalf("ResolveIn = %q, %v", got, err)
	}
	if reqs := v.got(); len(reqs) != 1 || reqs[0].fields[1] != "NPM_TOKEN" {
		t.Errorf("requests = %+v, want one get of entry NPM_TOKEN", reqs)
	}
}

func TestOpenVault_AMachineWideNameAsksTheServiceNothing(t *testing.T) {
	v := newFakeVault(t, vaultOf(map[string]string{"app/TOKEN": "x"}))
	p := NewOpenVaultProvider(v.socket, 0)

	_, err := p.Resolve(context.Background(), "TOKEN")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if reqs := v.got(); len(reqs) != 0 {
		t.Errorf("requests = %+v; a machine-wide name has no collection, so nothing should be asked", reqs)
	}

	// Nor does a stopped service turn a machine-wide name into a failure: a
	// jobs.yaml job reads only those.
	stopped := NewOpenVaultProvider(filepath.Join(t.TempDir(), "absent.sock"), 0)
	if _, err := stopped.Resolve(context.Background(), "TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stopped service, machine-wide name: err = %v, want ErrNotFound", err)
	}
}

func TestOpenVault_AnEntryTheCollectionLacksIsAMiss(t *testing.T) {
	v := newFakeVault(t, vaultOf(map[string]string{"app/OTHER": "x"}))
	_, err := NewOpenVaultProvider(v.socket, 0).ResolveIn(context.Background(), []string{"app", "staging"}, "TOKEN")
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoCollection) {
		t.Fatalf("err = %v, want ErrNotFound and not ErrNoCollection", err)
	}
	if !strings.Contains(err.Error(), "app has no staging/TOKEN") {
		t.Errorf("err = %q, want it to name the entry", err)
	}
}

func TestOpenVault_AMachineNotScopedToTheCollectionIsAMissThatNamesIt(t *testing.T) {
	v := newFakeVault(t, vaultOf(map[string]string{"latem-cloud/TOKEN": "x"}))
	_, err := NewOpenVaultProvider(v.socket, 0).ResolveIn(context.Background(), []string{"wake"}, "TOKEN")
	if !errors.Is(err, ErrNoCollection) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNoCollection, which is ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "wake") {
		t.Errorf("err = %q, want the collection named", err)
	}
	// get was refused, and collections told the two refusals apart.
	if reqs := v.got(); len(reqs) != 2 || reqs[1].typ != ovCollections {
		t.Errorf("requests = %+v, want get then collections", reqs)
	}
}

func TestOpenVault_ARefusalOnACollectionItHoldsIsABackendFailureAndKeepsItsWordsLocal(t *testing.T) {
	v := newFakeVault(t, func(r ovRequest) []byte {
		if r.typ == ovCollections {
			return ovNameList("app")
		}
		return ovRefusal("this device has no key for collection 1f2e")
	})
	_, err := NewOpenVaultProvider(v.socket, 0).ResolveIn(context.Background(), []string{"app"}, "TOKEN")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a failure that is not a miss", err)
	}
	var oe *OpenVaultError
	if !errors.As(err, &oe) || !oe.Refused {
		t.Fatalf("err = %#v, want a refused OpenVaultError", err)
	}
	if strings.Contains(err.Error(), "no key") {
		t.Errorf("Error() = %q carries the service's message, which can reach a broker", err)
	}
	if !strings.Contains(oe.Diagnostic(), "no key for collection") {
		t.Errorf("Diagnostic() = %q, want the service's words", oe.Diagnostic())
	}
	if code := (&Error{Err: err}).Code(); code != protocol.ErrSecretBackend {
		t.Errorf("code = %s, want %s", code, protocol.ErrSecretBackend)
	}
}

func TestOpenVault_AServiceThatIsNotRunningIsABackendFailureNotAMiss(t *testing.T) {
	stale := ovSocketPath(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	// What a crashed service leaves: the socket, and nothing listening.
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()

	for name, socket := range map[string]string{
		"no socket":    filepath.Join(filepath.Dir(stale), "absent.sock"),
		"stale socket": stale,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewOpenVaultProvider(socket, 0).ResolveIn(context.Background(), []string{"app"}, "TOKEN")
			var oe *OpenVaultError
			if !errors.As(err, &oe) || !oe.NotRunning {
				t.Fatalf("err = %v, want a not-running OpenVaultError", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Error("a stopped service read as a miss: a workflow job would get an empty secret")
			}
			if !strings.Contains(err.Error(), "openvault service") {
				t.Errorf("err = %q, want the command that starts it", err)
			}
			if strings.Contains(err.Error(), socket) {
				t.Errorf("Error() = %q names a local path, which can reach a broker", err)
			}
		})
	}
}

func TestOpenVault_ResolveAllReportsAStoppedServiceAsARetryableBackendFailure(t *testing.T) {
	chain := NewChain(NewOpenVaultProvider(filepath.Join(t.TempDir(), "absent.sock"), 0))
	_, err := ResolveAll(context.Background(), scopedOnly{chain, []string{"app"}}, &fakeRegistrar{}, []string{"TOKEN"})
	if code := CodeOf(err); code != protocol.ErrSecretBackend {
		t.Fatalf("code = %q (err %v), want %s", code, err, protocol.ErrSecretBackend)
	}
}

// scopedOnly resolves every name under one scope, as a workflow job's first
// lookup does, so ResolveAll can be pointed at a scoped store.
type scopedOnly struct {
	c     *Chain
	scope []string
}

func (s scopedOnly) Resolve(ctx context.Context, name string) (Value, error) {
	return s.c.ResolveIn(ctx, s.scope, name)
}
func (s scopedOnly) Name() string { return "scoped" }

func TestOpenVault_AServiceThatNeverAnswersTimesOut(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	v := newFakeVault(t, func(ovRequest) []byte { <-block; return nil })

	start := time.Now()
	_, err := NewOpenVaultProvider(v.socket, 100*time.Millisecond).
		ResolveIn(context.Background(), []string{"app"}, "TOKEN")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %q", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to give up", elapsed)
	}
}

func TestOpenVault_AValueIsItsExactBytes(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nMIIE\x00\xff\n-----END PRIVATE KEY-----\n\n"
	v := newFakeVault(t, func(r ovRequest) []byte {
		if r.typ == ovGet {
			return ovFound(ovVar, pem)
		}
		return ovNameList("app")
	})
	got, err := NewOpenVaultProvider(v.socket, 0).ResolveIn(context.Background(), []string{"app"}, "KEY")
	if err != nil {
		t.Fatal(err)
	}
	// No trailing newline is the tool's to trim: the value is framed.
	if string(got) != pem {
		t.Errorf("value = %q, want %q", got, pem)
	}
}

func TestOpenVault_AnAnswerThatDoesNotParseIsABackendFailure(t *testing.T) {
	for name, resp := range map[string][]byte{
		"truncated value": {ovEntry, 1, ovSecret, 0, 0, 0, 9, 'x'},
		"trailing bytes":  append(ovFound(ovSecret, "value-0123456789"), 0),
		"unknown type":    {0x99},
		"a file":          ovFound(0x03, "manifest"),
		"found is 2":      {ovEntry, 2},
	} {
		t.Run(name, func(t *testing.T) {
			v := newFakeVault(t, func(ovRequest) []byte { return resp })
			got, err := NewOpenVaultProvider(v.socket, 0).ResolveIn(context.Background(), []string{"app"}, "KEY")
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("ResolveIn = %q, %v; want a failure that is not a miss", got, err)
			}
		})
	}
}

func TestOpenVault_AMessageOverTheServicesLimitIsRefusedUnread(t *testing.T) {
	socket := ovSocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.ReadFull(conn, make([]byte, 4+1+2+3+2+3))
		_, _ = conn.Write(binary.BigEndian.AppendUint32(nil, openVaultMaxFrame+1))
	}()
	_, err = NewOpenVaultProvider(socket, 0).ResolveIn(context.Background(), []string{"app"}, "KEY")
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("err = %v, want the oversized message refused", err)
	}
}

func TestOpenVault_APathThatIsNotASocketIsNotAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), OpenVaultSocketName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewOpenVaultProvider(path, 0).ResolveIn(context.Background(), []string{"app"}, "KEY")
	var oe *OpenVaultError
	if !errors.As(err, &oe) || oe.NotRunning || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("err = %v, want a refusal to read a file as a socket", err)
	}
	if strings.Contains(err.Error(), path) || !strings.Contains(oe.Diagnostic(), path) {
		t.Errorf("Error() = %q, Diagnostic() = %q; the path belongs in the diagnostic only", err, oe.Diagnostic())
	}
}

func TestOpenVaultEntry(t *testing.T) {
	for _, tc := range []struct {
		scope             []string
		collection, entry string
	}{
		{[]string{"latem-cloud", "production"}, "latem-cloud", "production/TOKEN"},
		{[]string{"ios-native", "prod.eu"}, "ios-native", "prod.eu/TOKEN"},
		{[]string{"1password-sync"}, "1password-sync", "TOKEN"},
		{[]string{"app", "github pages"}, "app", "github pages/TOKEN"},
	} {
		c, e, err := OpenVaultEntry(tc.scope, "TOKEN")
		if err != nil || c != tc.collection || e != tc.entry {
			t.Errorf("OpenVaultEntry(%q) = %q, %q, %v; want %q, %q", tc.scope, c, e, err, tc.collection, tc.entry)
		}
	}
	for _, scope := range [][]string{
		nil,
		{"-app"},
		{".app"},
		{"app/x"},
		{strings.Repeat("a", 65)},
		{"app", ""},
		{"app", ".."},
		{"app", "a/b"},
		{"app", "a\nb"},
		{"app", "env", "extra"},
	} {
		if c, e, err := OpenVaultEntry(scope, "TOKEN"); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenVaultEntry(%q) = %q, %q, %v; want ErrNotFound", scope, c, e, err)
		}
	}
}

func TestOpenVault_NamesAreTheScopedNamesARunLooksUpAndOnlyForTheRepositoriesAsked(t *testing.T) {
	v := newFakeVault(t, func(r ovRequest) []byte {
		switch {
		case r.typ == ovCollections:
			return ovNameList("app", "ssh")
		case r.typ == ovList && r.fields[0] == "app":
			return ovListingOf(map[string]byte{
				"NPM_TOKEN":          ovSecret,
				"production/DEPLOY":  ovSecret,
				"staging/SITE_URL":   ovVar,
				"release/upload.jks": 0x03, // a file
				"a/b/DEEP":           ovSecret,
				"production/bad-nm":  ovSecret,
			})
		}
		return ovRefusal("unexpected")
	})
	names, absent, err := NewOpenVaultProvider(v.socket, 0).Names(context.Background(), []string{"wake", "app"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app__NPM_TOKEN", "app__production__DEPLOY", "app__staging__SITE_URL"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if !slices.Equal(absent, []string{"wake"}) {
		t.Errorf("absent = %v, want [wake]", absent)
	}
	for _, r := range v.got() {
		if r.typ == ovList && r.fields[0] != "app" {
			t.Errorf("listed %s, which is not a repository it was asked about", r.fields[0])
		}
	}
}

func TestOpenVaultSocket(t *testing.T) {
	for _, tc := range []struct {
		environ []string
		want    string
	}{
		{[]string{"HOME=/home/m"}, "/home/m/.openvault/service.sock"},
		{[]string{"HOME=/home/m", "OPENVAULT_HOME=/srv/ov"}, "/srv/ov/service.sock"},
	} {
		if got, err := OpenVaultSocket(tc.environ); err != nil || got != tc.want {
			t.Errorf("OpenVaultSocket(%q) = %q, %v; want %q", tc.environ, got, err, tc.want)
		}
	}
	if _, err := OpenVaultSocket(nil); err == nil {
		t.Error("no HOME and no OPENVAULT_HOME should be an error, not a socket in the working directory")
	}
}

func ovRequestEqual(a, b ovRequest) bool { return a.typ == b.typ && slices.Equal(a.fields, b.fields) }
