// Package conformance replays a committed corpus of wire frames against the
// live remote broker, in both directions.
//
// # Why this exists
//
// The control plane used to live in this repository, and frames from each side
// were fed through the other's decoder. That caught real encoding mistakes in
// both. The hosted layer is now a separate repository (PLAN §5, RUNNER_SPEC
// §26.6), so nothing in this tree exercises the far end of the wire, and
// docs/PROTOCOL.md §8 records the gap in as many words: what remains here is
// the strictness of protocol.Command.UnmarshalJSON, which stops a hostile frame
// being *accepted* and says nothing about whether the frames this agent *emits*
// are the ones a server expects.
//
// So: a corpus, committed, replayed here. A change to
// internal/broker/remote/wire.go now fails a test in this repository rather
// than surfacing as a decode error in another one.
//
// # What makes this a conformance test rather than a round-trip
//
// Nothing here imports internal/broker/remote's own `frame`, `encode` or
// `decode`. It could not — they are unexported, deliberately — and it should
// not, because a test that encodes and decodes with the same code proves only
// that the code is self-consistent. Both directions go through an independent
// reader: raw bytes off a real WebSocket on the way out, hand-written JSON on
// the way in. That is the property the deleted cross-check had, and it is the
// only reason it caught anything.
//
// # The corpus is canonicalised, not raw
//
// Every recorded frame is decoded to a generic value, has its volatile parts
// replaced (the envelope's per-session correlation id, and every RFC 3339
// timestamp), and is re-encoded with sorted keys. Nothing semantic is lost:
// JSON object order is not meaningful, while absent-versus-null and
// number-versus-string — the two distinctions docs/PROTOCOL.md §5 is written
// about — both survive a map round trip. What it buys is a corpus that is
// stable enough to diff by eye in a pull request.
//
// Regenerate with `go test ./test/conformance -update`.
package conformance

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/internal/store"
)

// update rewrites the committed corpus from what the agent emitted.
var update = flag.Bool("update", false, "rewrite testdata/*.jsonl from this run")

// ---------------------------------------------------------------------------
// a control plane that records bytes

// recorder is a WebSocket endpoint that keeps every message verbatim.
//
// It decodes as little as it can get away with — enough to find the envelope
// kind and correlation id so it can acknowledge, and no further. Anything more
// would be a second implementation of the format, which is the thing whose
// absence this package is compensating for.
type recorder struct {
	t     *testing.T
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	received [][]byte
	conns    []*websocket.Conn
	// hello, when set, is pushed as the first server frame on every connect.
	hello []byte
	// autoAck answers any framed request carrying an id.
	autoAck bool
	waiters []chan struct{}
}

func newRecorder(t *testing.T, token string) *recorder {
	t.Helper()
	r := &recorder{t: t, token: token, autoAck: true}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) URL() string { return r.srv.URL }

func (r *recorder) serve(w http.ResponseWriter, req *http.Request) {
	if r.token != "" && req.Header.Get("Authorization") != "Bearer "+r.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(4 << 20)

	r.mu.Lock()
	r.conns = append(r.conns, conn)
	hello := r.hello
	r.mu.Unlock()

	if hello != nil {
		_ = conn.Write(req.Context(), websocket.MessageText, hello)
	}

	for {
		typ, data, rerr := conn.Read(req.Context())
		if rerr != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		r.record(data)
		r.maybeAck(req.Context(), conn, data)
	}
}

func (r *recorder) record(data []byte) {
	r.mu.Lock()
	r.received = append(r.received, append([]byte(nil), data...))
	waiters := r.waiters
	r.waiters = nil
	r.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// maybeAck answers a request frame.
//
// The only fields read are `kind` and `id`, from a generic decode. A server
// that had to understand the payload to acknowledge it would be re-deriving
// the format, and the whole point here is to hold the format at arm's length.
func (r *recorder) maybeAck(ctx context.Context, conn *websocket.Conn, data []byte) {
	r.mu.Lock()
	auto := r.autoAck
	r.mu.Unlock()
	if !auto {
		return
	}

	var env struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.ID == "" {
		return
	}
	switch env.Kind {
	case "claim", "update", "logs":
	default:
		return // resume, heartbeat and ack want no answer (PROTOCOL §2.1)
	}
	ack := fmt.Sprintf(`{"kind":"ack","id":%q,"protocol_version":"1.0","ack":{"ok":true}}`, env.ID)
	_ = conn.Write(ctx, websocket.MessageText, []byte(ack))
}

// push sends raw bytes to every live connection, which is how a server frame
// fixture is delivered without this package owning an encoder.
func (r *recorder) push(t *testing.T, raw []byte) {
	t.Helper()
	r.mu.Lock()
	conns := append([]*websocket.Conn(nil), r.conns...)
	r.mu.Unlock()
	if len(conns) == 0 {
		t.Fatal("nothing is connected; the fixture cannot be delivered")
	}
	for _, c := range conns {
		if err := c.Write(context.Background(), websocket.MessageText, raw); err != nil {
			t.Fatalf("pushing a fixture: %v", err)
		}
	}
}

// frames returns everything received so far, verbatim.
func (r *recorder) frames() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.received...)
}

// waitForKind blocks until a frame of the given kind has been received, and
// returns it.
func (r *recorder) waitForKind(t *testing.T, kind string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, f := range r.frames() {
			if kindOf(f) == kind {
				return f
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s frame arrived within %s; received %v", kind, timeout, kindsOf(r.frames()))
	return nil
}

func kindOf(raw []byte) string {
	var env struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return env.Kind
}

func kindsOf(raws [][]byte) []string {
	out := make([]string, 0, len(raws))
	for _, r := range raws {
		out = append(out, kindOf(r))
	}
	return out
}

// ---------------------------------------------------------------------------
// canonicalisation

// rfc3339 is deliberately loose about fractional precision, because Go writes
// a variable number of digits and docs/PROTOCOL.md §5.4 names that as a trap.
var rfc3339 = regexp.MustCompile(
	`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`)

const (
	placeholderID = "__correlation_id__"
	placeholderTS = "__rfc3339__"
)

// canonicalise decodes, replaces the volatile parts, and re-encodes.
func canonicalise(t *testing.T, raw []byte) string {
	t.Helper()

	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber() // so a uint64 sequence is not rounded through a float64
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("the agent emitted something that is not JSON: %s", raw)
	}

	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("the agent emitted a %T at the top level, want an object: %s", v, raw)
	}
	if _, has := obj["id"]; has {
		obj["id"] = placeholderID
	}
	scrubTimestamps(v)

	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// scrubTimestamps replaces every RFC 3339 string anywhere in the tree.
func scrubTimestamps(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if s, ok := child.(string); ok && rfc3339.MatchString(s) {
				t[k] = placeholderTS
				continue
			}
			scrubTimestamps(child)
		}
	case []any:
		for i, child := range t {
			if s, ok := child.(string); ok && rfc3339.MatchString(s) {
				t[i] = placeholderTS
				continue
			}
			scrubTimestamps(child)
		}
	}
}

// ---------------------------------------------------------------------------
// the corpus files

// corpusPath is testdata/<name>.
func corpusPath(name string) string { return filepath.Join("testdata", name) }

// readCorpus loads a JSONL file, ignoring blank lines and `//` comments.
func readCorpus(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(corpusPath(name))
	if err != nil {
		t.Fatalf("reading the corpus: %v\nregenerate with: go test ./test/conformance -update", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// writeCorpus rewrites a JSONL file under -update.
func writeCorpus(t *testing.T, name string, lines []string, header string) {
	t.Helper()
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	body := header + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(corpusPath(name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("rewrote %s (%d frames)", corpusPath(name), len(lines))
}

// ---------------------------------------------------------------------------
// odds and ends

// shortTemp keeps paths inside the 103-byte Unix socket budget. Nothing here
// binds a socket, but the store and the token file sit beside things that do
// in production, and a fixture that only works in one directory is a fixture
// waiting to move.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "conf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func openStore(t *testing.T, dir string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(dir, "state.db"), noRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type noRedactor struct{}

func (noRedactor) Redact(line string) string { return line }

func writeTokenFile(t *testing.T, dir, token string) string {
	t.Helper()
	path := filepath.Join(dir, "agent.key")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sortedUnique is how a corpus is compared: as a set, because the order frames
// reach the wire is a scheduling detail and the corpus is about their shape.
func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
