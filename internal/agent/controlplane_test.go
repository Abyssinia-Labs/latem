package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// controlPlane is a WebSocket control plane for the tests that need both
// transports live at once.
//
// internal/broker/remote has its own, richer fake, and this is deliberately not
// it: that one is unexported test code in another package, and copying its
// knobs here would be copying a design for testing one transport in isolation.
// What this one exists for is the opposite — the agent with a local broker and a
// remote broker running together — so it implements exactly the server side of
// §7.4 and one operation the transport-level fake does not need: a partition
// that a real, running agent has to reconnect through on its real backoff
// schedule.
//
// It also implements §7.2's dedupe rule, because that rule is half of the M6
// criterion. The server "keeps the first frame it sees for a given (run_id,
// seq) and discards later ones", and a test that did not do that could not tell
// a broker which re-sent its whole buffer from one that resumed correctly.
type controlPlane struct {
	srv *httptest.Server

	mu          sync.Mutex
	partitioned bool
	conns       []*websocket.Conn
	handshakes  int
	refusals    int
	resumes     []protocol.Resume
	updates     []cpUpdate
	frames      map[string][]protocol.LogFrame
	seen        map[string]map[uint64]bool
	duplicates  map[string]int
	heartbeats  int
}

type cpUpdate struct {
	RunID  string          `json:"run_id"`
	Update protocol.Update `json:"update"`
}

// cpFrame is the server's view of internal/broker/remote's envelope. That type
// is unexported there, and correctly so — RUNNER_SPEC §4 puts wire types in
// pkg/protocol and pkg/protocol has no envelope, which is recorded as a contract
// defect in the remote package's comment. The consequence lands here: anything
// speaking to that transport has to restate the envelope. This is the second
// copy, and it is a test, which is the cheapest place for it to be.
type cpFrame struct {
	Kind      string                  `json:"kind"`
	ID        string                  `json:"id,omitempty"`
	Version   string                  `json:"protocol_version,omitempty"`
	Hello     *cpHello                `json:"hello,omitempty"`
	Command   *protocol.Command       `json:"command,omitempty"`
	Ack       *protocol.Ack           `json:"ack,omitempty"`
	Resume    *protocol.Resume        `json:"resume,omitempty"`
	Update    *cpUpdate               `json:"update,omitempty"`
	Logs      []protocol.LogFrame     `json:"logs,omitempty"`
	Heartbeat *protocol.MachineHealth `json:"heartbeat,omitempty"`
	Claim     *cpClaim                `json:"claim,omitempty"`
}

type cpHello struct {
	MachineID string `json:"machine_id,omitempty"`
	Version   string `json:"protocol_version,omitempty"`
	Message   string `json:"message,omitempty"`
}

type cpClaim struct {
	RunID string `json:"run_id"`
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()

	cp := &controlPlane{
		frames:     map[string][]protocol.LogFrame{},
		seen:       map[string]map[uint64]bool{},
		duplicates: map[string]int{},
	}
	cp.srv = httptest.NewServer(http.HandlerFunc(cp.handle))
	t.Cleanup(func() {
		cp.closeAll()
		cp.srv.Close()
	})
	return cp
}

// URL is the ws:// address for `[broker.remote] url`. The config loader permits
// plain ws only to a loopback host, which httptest is.
func (cp *controlPlane) URL() string {
	return "ws" + strings.TrimPrefix(cp.srv.URL, "http") + "/agent"
}

func (cp *controlPlane) handle(w http.ResponseWriter, r *http.Request) {
	cp.mu.Lock()
	down := cp.partitioned
	if down {
		cp.refusals++
	}
	cp.mu.Unlock()
	if down {
		// A partition, from the agent's side: the dial fails and the reconnect
		// loop backs off. Not 401 or 403, which would be a revoked token and
		// would stop the loop for good.
		http.Error(w, "partitioned", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)

	cp.mu.Lock()
	cp.handshakes++
	cp.conns = append(cp.conns, conn)
	cp.mu.Unlock()

	ctx := context.Background()
	_ = cp.send(ctx, conn, cpFrame{
		Kind:  "hello",
		Hello: &cpHello{MachineID: "mch_test", Version: protocol.Version},
	})

	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var f cpFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		cp.dispatch(ctx, conn, f)
	}
}

func (cp *controlPlane) dispatch(ctx context.Context, conn *websocket.Conn, f cpFrame) {
	switch f.Kind {
	case "resume":
		if f.Resume != nil {
			cp.mu.Lock()
			cp.resumes = append(cp.resumes, *f.Resume)
			cp.mu.Unlock()
		}

	case "update":
		if f.Update != nil {
			cp.mu.Lock()
			cp.updates = append(cp.updates, *f.Update)
			cp.mu.Unlock()
			cp.ack(ctx, conn, f.ID, protocol.Ack{OK: true, RunID: f.Update.RunID})
		}

	case "logs":
		cp.ingest(f.Logs)
		cp.ack(ctx, conn, f.ID, protocol.Ack{OK: true})

	case "heartbeat":
		cp.mu.Lock()
		cp.heartbeats++
		cp.mu.Unlock()

	case "claim":
		id := ""
		if f.Claim != nil {
			id = f.Claim.RunID
		}
		cp.ack(ctx, conn, f.ID, protocol.Ack{OK: true, RunID: id})
	}
}

// ingest applies §7.2's dedupe: the first frame for a (run_id, seq) is kept and
// later ones are counted and discarded.
func (cp *controlPlane) ingest(frames []protocol.LogFrame) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for _, fr := range frames {
		seen := cp.seen[fr.RunID]
		if seen == nil {
			seen = map[uint64]bool{}
			cp.seen[fr.RunID] = seen
		}
		if seen[fr.Seq] {
			cp.duplicates[fr.RunID]++
			continue
		}
		seen[fr.Seq] = true
		cp.frames[fr.RunID] = append(cp.frames[fr.RunID], fr)
	}
}

func (cp *controlPlane) ack(ctx context.Context, conn *websocket.Conn, id string, a protocol.Ack) {
	if id == "" {
		return
	}
	_ = cp.send(ctx, conn, cpFrame{Kind: "ack", ID: id, Ack: &a})
}

func (cp *controlPlane) send(ctx context.Context, conn *websocket.Conn, f cpFrame) error {
	f.Version = protocol.Version
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

// push sends a command down the live connection, which is how a control plane
// starts a build on this machine (§7.1).
func (cp *controlPlane) push(t *testing.T, cmd protocol.Command) {
	t.Helper()

	cp.mu.Lock()
	conns := append([]*websocket.Conn(nil), cp.conns...)
	cp.mu.Unlock()
	if len(conns) == 0 {
		t.Fatal("no agent is connected to the control plane")
	}
	// The newest connection: an earlier one may be a session the agent has
	// already retired.
	conn := conns[len(conns)-1]
	if err := cp.send(context.Background(), conn, cpFrame{
		Kind:    "command",
		ID:      "cmd-1",
		Command: &cmd,
	}); err != nil {
		t.Fatalf("pushing a command: %v", err)
	}
}

// Partition drops every live connection and refuses new ones until Heal.
func (cp *controlPlane) Partition() {
	cp.mu.Lock()
	cp.partitioned = true
	cp.mu.Unlock()
	cp.closeAll()
}

// Heal lets the agent back in.
func (cp *controlPlane) Heal() {
	cp.mu.Lock()
	cp.partitioned = false
	cp.mu.Unlock()
}

func (cp *controlPlane) closeAll() {
	cp.mu.Lock()
	conns := cp.conns
	cp.conns = nil
	cp.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}
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

func (cp *controlPlane) Resumes() []protocol.Resume {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]protocol.Resume(nil), cp.resumes...)
}

func (cp *controlPlane) Updates() []cpUpdate {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]cpUpdate(nil), cp.updates...)
}

// Seqs returns the sequence numbers the control plane holds for one run, in
// arrival order and already deduped.
func (cp *controlPlane) Seqs(runID string) []uint64 {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]uint64, 0, len(cp.frames[runID]))
	for _, fr := range cp.frames[runID] {
		out = append(out, fr.Seq)
	}
	return out
}

func (cp *controlPlane) Duplicates(runID string) int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.duplicates[runID]
}

// HasState reports whether a run has been reported in a given state.
func (cp *controlPlane) HasState(runID string, state protocol.RunState) bool {
	for _, u := range cp.Updates() {
		if u.RunID == runID && u.Update.State == state {
			return true
		}
	}
	return false
}
