package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Everything in this package's tests talks over a real WebSocket to a real
// httptest server backed by a real sqlite database. Nothing about the transport
// is mocked, because everything interesting about it — the handshake, the 401,
// the connection dying mid-batch, the order frames arrive in — is a property of
// the network stack rather than of this package's control flow, and a fake would
// assert only that the code calls the functions it calls.
//
// The one thing that *is* injected is the clock, and only in the reconnect loop.
// See [fakeClock].

// ---------------------------------------------------------------------------
// the fake control plane

// controlPlane is a conformant-enough server: it authenticates, acknowledges,
// replays, and — the part that matters — deduplicates log frames by
// (run_id, seq) and keeps the first, exactly as RUNNER_SPEC §7.2 requires of a
// real one. Without that rule on this side the client's resume behaviour cannot
// be judged, because "the agent re-offered a batch whose acknowledgement was
// lost" and "the agent duplicated the outage" look identical.
type controlPlane struct {
	t     *testing.T
	srv   *httptest.Server
	token string

	mu          sync.Mutex
	partitioned bool
	conns       map[*websocket.Conn]struct{}
	handshakes  int
	refusals    int

	resumes    []protocol.Resume
	updates    []recordedUpdate
	heartbeats []protocol.MachineHealth
	plans      []protocol.PlanResult
	claims     []string
	// commandAcks are the agent's answers to commands this control plane sent.
	commandAcks []protocol.Ack

	// logs holds the first frame seen for each (run, seq), in arrival order.
	logs map[string][]protocol.LogFrame
	seen map[string]map[uint64]bool
	// duplicates counts frames refused by the §7.2 dedupe rule.
	duplicates map[string]int

	// knobs, read under mu.
	withholdLogAcks bool
	// swallowLogs discards a batch entirely: not recorded, not acknowledged.
	// It is the other half of a partition landing on an in-flight write — the
	// bytes went into a socket that died — and it is the half that proves the
	// agent re-offers rather than losing them.
	swallowLogs   bool
	refuseClaims  bool
	refuseUpdates bool
	hello         *helloFrame
	onResume      func(cp *controlPlane, c *websocket.Conn, r protocol.Resume)

	// waiters is how a test blocks until something has happened without
	// sleeping for it.
	waiters []chan struct{}
}

type recordedUpdate struct {
	RunID  string
	Update protocol.Update
}

func newControlPlane(t *testing.T, token string) *controlPlane {
	t.Helper()
	cp := &controlPlane{
		t:          t,
		token:      token,
		conns:      map[*websocket.Conn]struct{}{},
		logs:       map[string][]protocol.LogFrame{},
		seen:       map[string]map[uint64]bool{},
		duplicates: map[string]int{},
	}
	cp.srv = httptest.NewServer(http.HandlerFunc(cp.serve))
	t.Cleanup(cp.srv.Close)
	return cp
}

// URL is what goes in `[broker.remote] url`. httptest speaks http://, which
// coder/websocket accepts as ws://; the config loader is what insists on wss://
// for anything that is not loopback, and that rule is its test, not this one's.
func (cp *controlPlane) URL() string { return cp.srv.URL }

func (cp *controlPlane) serve(w http.ResponseWriter, r *http.Request) {
	cp.mu.Lock()
	partitioned, token := cp.partitioned, cp.token
	cp.mu.Unlock()

	if partitioned {
		// A partition, from the agent's side, is indistinguishable from a
		// gateway that cannot reach the origin. It is deliberately not a refused
		// TCP connection: a 502 proves the reconnect loop treats an HTTP-level
		// failure as retryable, which is the case a load balancer produces.
		cp.count(&cp.refusals)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		cp.count(&cp.refusals)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	cp.count(&cp.handshakes)
	cp.addConn(conn)
	defer cp.removeConn(conn)

	cp.mu.Lock()
	hello := cp.hello
	cp.mu.Unlock()
	if hello != nil {
		cp.push(conn, frame{Kind: kindHello, Hello: hello})
	}

	ctx := r.Context()
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		cp.handle(conn, data)
	}
}

func (cp *controlPlane) handle(conn *websocket.Conn, data []byte) {
	var f frame
	if err := decodeAny(data, &f); err != nil {
		cp.t.Errorf("control plane could not decode a frame from the agent: %v", err)
		return
	}
	if f.Version != protocol.Version {
		cp.t.Errorf("frame %s carried protocol version %q, want %q", f.Kind, f.Version, protocol.Version)
	}

	switch f.Kind {
	case kindResume:
		cp.mu.Lock()
		cp.resumes = append(cp.resumes, *f.Resume)
		hook := cp.onResume
		cp.mu.Unlock()
		cp.notify()
		if hook != nil {
			hook(cp, conn, *f.Resume)
		}

	case kindHeartbeat:
		cp.mu.Lock()
		cp.heartbeats = append(cp.heartbeats, *f.Heartbeat)
		cp.mu.Unlock()
		cp.notify()

	case kindClaim:
		cp.mu.Lock()
		cp.claims = append(cp.claims, f.Claim.RunID)
		refuse := cp.refuseClaims
		cp.mu.Unlock()
		cp.notify()
		ack := protocol.Ack{OK: !refuse, RunID: f.Claim.RunID}
		if refuse {
			ack.Error = "another runner claimed it first"
		}
		cp.push(conn, frame{Kind: kindAck, ID: f.ID, Ack: &ack})

	case kindUpdate:
		cp.mu.Lock()
		cp.updates = append(cp.updates, recordedUpdate{RunID: f.Update.RunID, Update: f.Update.Update})
		refuse := cp.refuseUpdates
		cp.mu.Unlock()
		cp.notify()
		ack := protocol.Ack{OK: !refuse, RunID: f.Update.RunID}
		if refuse {
			ack.Error = "no"
		}
		cp.push(conn, frame{Kind: kindAck, ID: f.ID, Ack: &ack})

	case kindLogs:
		cp.mu.Lock()
		swallow := cp.swallowLogs
		cp.mu.Unlock()
		if swallow {
			return
		}
		high := cp.recordLogs(f.Logs)
		cp.mu.Lock()
		withhold := cp.withholdLogAcks
		cp.mu.Unlock()
		if withhold {
			// The frames are held but never acknowledged: the case a partition
			// produces when it lands between the write and the reply.
			return
		}
		ack := protocol.Ack{OK: true}
		if len(f.Logs) > 0 {
			ack.RunID = f.Logs[0].RunID
		}
		_ = high
		cp.push(conn, frame{Kind: kindAck, ID: f.ID, Ack: &ack})

	case kindPlan:
		cp.mu.Lock()
		cp.plans = append(cp.plans, *f.Plan)
		cp.mu.Unlock()
		cp.notify()
		cp.push(conn, frame{Kind: kindAck, ID: f.ID, Ack: &protocol.Ack{OK: true}})

	case kindAck:
		// The agent's answer to a command this control plane sent.
		cp.mu.Lock()
		cp.commandAcks = append(cp.commandAcks, *f.Ack)
		cp.mu.Unlock()
		cp.notify()

	default:
		cp.t.Errorf("the agent sent a %q frame, which is not part of its outbound vocabulary", f.Kind)
	}
}

// recordLogs applies §7.2's dedupe rule and returns the highest sequence in the
// batch.
func (cp *controlPlane) recordLogs(frames []protocol.LogFrame) uint64 {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var high uint64
	for _, f := range frames {
		if f.Seq > high {
			high = f.Seq
		}
		seen := cp.seen[f.RunID]
		if seen == nil {
			seen = map[uint64]bool{}
			cp.seen[f.RunID] = seen
		}
		if seen[f.Seq] {
			cp.duplicates[f.RunID]++
			continue
		}
		seen[f.Seq] = true
		cp.logs[f.RunID] = append(cp.logs[f.RunID], f)
	}
	cp.notifyLocked()
	return high
}

func (cp *controlPlane) push(conn *websocket.Conn, f frame) {
	data, err := encode(f)
	if err != nil {
		cp.t.Errorf("control plane could not encode %s: %v", f.Kind, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

// Command sends one command to every connected agent, as a control plane
// replaying a backlog would.
func (cp *controlPlane) Command(cmd protocol.Command) {
	for _, c := range cp.liveConns() {
		cp.push(c, frame{Kind: kindCommand, ID: "srv-1", Command: &cmd})
	}
}

// Partition makes the control plane unreachable and drops every live
// connection, which is what a network partition looks like from the agent.
func (cp *controlPlane) Partition() {
	cp.mu.Lock()
	cp.partitioned = true
	cp.mu.Unlock()
	for _, c := range cp.liveConns() {
		_ = c.CloseNow()
	}
}

// Heal ends the partition.
func (cp *controlPlane) Heal() {
	cp.mu.Lock()
	cp.partitioned = false
	cp.mu.Unlock()
}

func (cp *controlPlane) WithholdLogAcks(v bool) {
	cp.mu.Lock()
	cp.withholdLogAcks = v
	cp.mu.Unlock()
}

func (cp *controlPlane) SwallowLogs(v bool) {
	cp.mu.Lock()
	cp.swallowLogs = v
	cp.mu.Unlock()
}

func (cp *controlPlane) RefuseClaims(v bool) {
	cp.mu.Lock()
	cp.refuseClaims = v
	cp.mu.Unlock()
}

func (cp *controlPlane) SetHello(h *helloFrame) {
	cp.mu.Lock()
	cp.hello = h
	cp.mu.Unlock()
}

func (cp *controlPlane) OnResume(fn func(cp *controlPlane, c *websocket.Conn, r protocol.Resume)) {
	cp.mu.Lock()
	cp.onResume = fn
	cp.mu.Unlock()
}

func (cp *controlPlane) liveConns() []*websocket.Conn {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]*websocket.Conn, 0, len(cp.conns))
	for c := range cp.conns {
		out = append(out, c)
	}
	return out
}

func (cp *controlPlane) addConn(c *websocket.Conn) {
	cp.mu.Lock()
	cp.conns[c] = struct{}{}
	cp.mu.Unlock()
	cp.notify()
}

func (cp *controlPlane) removeConn(c *websocket.Conn) {
	cp.mu.Lock()
	delete(cp.conns, c)
	cp.mu.Unlock()
	cp.notify()
}

func (cp *controlPlane) count(p *int) {
	cp.mu.Lock()
	*p++
	cp.mu.Unlock()
	cp.notify()
}

// Frames returns the deduplicated frames the control plane holds for a run, in
// the order it first saw them.
func (cp *controlPlane) Frames(runID string) []protocol.LogFrame {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]protocol.LogFrame, len(cp.logs[runID]))
	copy(out, cp.logs[runID])
	return out
}

// Duplicates is how many frames the §7.2 dedupe rule discarded.
func (cp *controlPlane) Duplicates(runID string) int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.duplicates[runID]
}

func (cp *controlPlane) Resumes() []protocol.Resume {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]protocol.Resume, len(cp.resumes))
	copy(out, cp.resumes)
	return out
}

func (cp *controlPlane) Updates() []recordedUpdate {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]recordedUpdate, len(cp.updates))
	copy(out, cp.updates)
	return out
}

func (cp *controlPlane) Heartbeats() []protocol.MachineHealth {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]protocol.MachineHealth, len(cp.heartbeats))
	copy(out, cp.heartbeats)
	return out
}

func (cp *controlPlane) Claims() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]string(nil), cp.claims...)
}

// CommandAcks are the agent's answers to the commands this control plane sent.
func (cp *controlPlane) CommandAcks() []protocol.Ack {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]protocol.Ack(nil), cp.commandAcks...)
}

func (cp *controlPlane) Handshakes() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.handshakes
}

func (cp *controlPlane) Refusals() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.refusals
}

// notify wakes anything blocked in [controlPlane.Await].
func (cp *controlPlane) notify() {
	cp.mu.Lock()
	cp.notifyLocked()
	cp.mu.Unlock()
}

func (cp *controlPlane) notifyLocked() {
	for _, ch := range cp.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Await blocks until cond returns true, or fails the test.
//
// It is event-driven rather than a poll loop with a sleep in it, because a test
// that sleeps is a test that is either slow or flaky and usually both.
func (cp *controlPlane) Await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ch := make(chan struct{}, 1)
	cp.mu.Lock()
	cp.waiters = append(cp.waiters, ch)
	cp.mu.Unlock()
	defer func() {
		cp.mu.Lock()
		for i, w := range cp.waiters {
			if w == ch {
				cp.waiters = append(cp.waiters[:i], cp.waiters[i+1:]...)
				break
			}
		}
		cp.mu.Unlock()
	}()

	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// decodeAny decodes a frame the agent sent. It is deliberately not [decode],
// which only accepts the two kinds a *control plane* may send — and it is
// strict for the reason the local broker's decodeReply is strict: a field this
// side does not know about means the two halves were built from different
// source, and that is worth a loud failure in a test above all places.
func decodeAny(data []byte, f *frame) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(f)
}

// ---------------------------------------------------------------------------
// the fake clock

// fakeClock replaces the reconnect loop's only wait.
//
// A ten-minute partition is ten minutes of *backoff*, not ten minutes of
// anything else: the loop has no other timer. So a clock that records what it
// was asked to wait for and returns immediately reproduces the schedule
// faithfully while the test runs in milliseconds.
//
// What that does NOT reproduce is stated where it matters, in the test: real
// TCP timeouts, TLS renegotiation, a server whose sockets linger in
// FIN_WAIT, and the possibility that ten real minutes of a build's output is
// more data than a short test emits. Those are covered, where they are covered
// at all, by the buffer-bound test rather than by wall clock.
type fakeClock struct {
	mu    sync.Mutex
	slept []time.Duration
	total time.Duration
	woken chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{woken: make(chan struct{}, 1024)}
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.total += d
	c.mu.Unlock()

	select {
	case c.woken <- struct{}{}:
	default:
	}

	// A token real wait, proportional and tiny. Returning instantly would be
	// closer to the fiction, and it would also turn the reconnect loop into a
	// spin that starves the test goroutine it is supposed to be observed by:
	// once the simulated ten minutes are up the loop keeps dialling until the
	// test heals the partition, and at full speed that is tens of thousands of
	// real HTTP requests. Two milliseconds at the cap keeps it to a few hundred
	// a second, which is fast enough to be invisible and slow enough to be
	// polite.
	real := d / 10000
	if real > 2*time.Millisecond {
		real = 2 * time.Millisecond
	}
	return realSleep(ctx, real)
}

// Total is how much simulated time the reconnect loop has waited for.
func (c *fakeClock) Total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Schedule is every delay, in order.
func (c *fakeClock) Schedule() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// ---------------------------------------------------------------------------
// fixtures

func newStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"), logstream.NewRedactor())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func newTokenFile(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")
	if err := WriteToken(path, token); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	return path
}

// seedRun creates a run row directly in the given state.
func seedRun(t *testing.T, db *store.DB, id string, state protocol.RunState) {
	t.Helper()
	err := db.Create(t.Context(), store.Run{
		ID:         id,
		Repo:       "myapp",
		Job:        "build",
		Ref:        "main",
		State:      state,
		OriginKind: "manual",
	})
	if err != nil {
		t.Fatalf("seeding run %s: %v", id, err)
	}
}

// harness is a broker wired to a control plane, with the knobs a test reaches
// for.
type harness struct {
	*Broker
	CP    *controlPlane
	DB    *store.DB
	Clock *fakeClock
	Token string
}

type harnessOptions struct {
	token    string
	logMode  LogMode
	maxBuf   int64
	queueTTL time.Duration
	// tokenFile overrides the generated one, for the revoked-token tests.
	tokenFile string
	// db reuses an existing database instead of creating one. It is how a test
	// simulates an agent restart: a new transport with no memory, against the
	// state the last one left behind.
	db *store.DB
}

func newHarness(t *testing.T, mutate func(*harnessOptions)) *harness {
	t.Helper()
	ho := harnessOptions{token: "tok-abc123"}
	if mutate != nil {
		mutate(&ho)
	}

	cp := newControlPlane(t, ho.token)
	db := ho.db
	if db == nil {
		db = newStore(t)
	}
	clock := newFakeClock()

	tokenFile := ho.tokenFile
	if tokenFile == "" {
		tokenFile = newTokenFile(t, ho.token)
	}

	b, err := Open(Options{
		URL:            cp.URL(),
		TokenFile:      tokenFile,
		MachineID:      "mch_test",
		Store:          db,
		LogMode:        ho.logMode,
		MaxBufferBytes: ho.maxBuf,
		QueueTTL:       ho.queueTTL,
		Backoff:        DefaultBackoff,
		Logger:         testLogger(t),
		sleep:          clock.sleep,
		// A fixed midpoint makes the schedule assertable. The dithering itself
		// has its own test.
		jitter: func() float64 { return 0.5 },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	return &harness{Broker: b, CP: cp, DB: db, Clock: clock, Token: ho.token}
}

// newHarnessOn builds a harness against an existing database, which is what an
// agent restart looks like from this package: the transport's buffers, sinks and
// pending reports are gone, and the only thing that carried over is the row.
func newHarnessOn(t *testing.T, db *store.DB, mutate func(*harnessOptions)) *harness {
	t.Helper()
	return newHarness(t, func(o *harnessOptions) {
		if mutate != nil {
			mutate(o)
		}
		o.db = db
	})
}

// AwaitConnected blocks until the broker has a live session.
func (h *harness) AwaitConnected(t *testing.T) {
	t.Helper()
	h.CP.Await(t, "the broker to connect", func() bool { return h.Connected() })
}

// AwaitResumes blocks until the control plane has seen n resume frames.
func (h *harness) AwaitResumes(t *testing.T, n int) {
	t.Helper()
	h.CP.Await(t, fmt.Sprintf("%d resume frames", n), func() bool { return len(h.CP.Resumes()) >= n })
}

// testLogger sends the transport's narration to the test log, so a failure
// carries the reconnect story with it.
//
// It is at warn level: the reconnect loop says something on every attempt, and
// a simulated ten-minute partition is fifteen of them.
func testLogger(t *testing.T) *slog.Logger {
	w := &testWriter{t: t}
	t.Cleanup(w.stop)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// testWriter forwards to t.Log, tolerating writes that arrive after the test
// has finished — the reconnect goroutine outlives the test body by a moment.
type testWriter struct {
	mu   sync.Mutex
	t    *testing.T
	done bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

func (w *testWriter) stop() {
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
}

// writeFrames pushes from..to into a sink as one batch.
func writeFrames(t *testing.T, s broker.LogSink, runID string, from, to uint64, line string) {
	t.Helper()
	batch := make([]protocol.LogFrame, 0, to-from+1)
	for seq := from; seq <= to; seq++ {
		batch = append(batch, protocol.LogFrame{
			RunID:  runID,
			Seq:    seq,
			Stream: protocol.StreamStdout,
			At:     time.Unix(0, int64(seq)).UTC(),
			Line:   fmt.Sprintf("%s %d", line, seq),
		})
	}
	if err := s.Write(context.Background(), batch); err != nil {
		t.Fatalf("sink.Write %d..%d: %v", from, to, err)
	}
}

// assertContiguous checks that the frames the control plane holds are exactly
// the sequences want, in order.
//
// It reports the first divergence rather than a diff of two long slices,
// because the failure this test exists to catch — a gap, a repeat, a reorder —
// is always describable in one sentence and never readable as two lists of 150
// numbers.
func assertContiguous(t *testing.T, got []protocol.LogFrame, want []uint64, runID string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("the control plane holds %d frames for %s, want %d", len(got), runID, len(want))
	}
	for i := range got {
		if i >= len(want) {
			t.Fatalf("frame %d is seq %d; nothing was expected past index %d", i, got[i].Seq, len(want)-1)
		}
		if got[i].Seq != want[i] {
			t.Fatalf("frame at index %d is seq %d, want %d — the stream %s at that point",
				i, got[i].Seq, want[i], describeDivergence(got[i].Seq, want[i]))
		}
		if got[i].RunID != runID {
			t.Fatalf("frame at index %d belongs to run %s, want %s", i, got[i].RunID, runID)
		}
	}
	if len(want) > len(got) {
		t.Fatalf("the control plane is missing sequences from %d onwards", want[len(got)])
	}
}

func describeDivergence(got, want uint64) string {
	switch {
	case got > want:
		return fmt.Sprintf("skipped %d sequence(s)", got-want)
	case got < want:
		return fmt.Sprintf("went backwards by %d", want-got)
	default:
		return "diverged"
	}
}

func seqRange(from, to uint64) []uint64 {
	out := make([]uint64, 0, to-from+1)
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

// tokenFileWith writes a token file at an explicit mode, for the mode checks.
func tokenFileWith(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	return path
}

// errorContains is the assertion this package's error messages are worth
// making: every one of them is supposed to name what to do next.
func errorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
}

// Plans are the plan answers the agent sent, in order.
func (cp *controlPlane) Plans() []protocol.PlanResult {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return slices.Clone(cp.plans)
}
