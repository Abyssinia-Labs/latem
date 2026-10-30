package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// controlPlane is a fake of latem-cloud's side of device registration, kept to
// the contract the two sides agreed: /claim, /claim/token, and the agent's
// WebSocket at /agent accepting the token the claim minted.
type controlPlane struct {
	srv *httptest.Server

	mu       sync.Mutex
	claims   []map[string]any // each /claim body, as sent
	polls    int
	answers  []string // /claim/token answers in order: an RFC 8628 error, or "token"
	interval int
	token    string
	id       string
	bearer   string // the Authorization the agent's handshake carried
}

func newControlPlane(t *testing.T, answers ...string) *controlPlane {
	t.Helper()
	cp := &controlPlane{answers: answers, interval: 2, token: "lt_machine_0123456789abcdef", id: "mch_claimed"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /claim", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		cp.mu.Lock()
		cp.claims = append(cp.claims, body)
		cp.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":               "dc_secret_device_code",
			"user_code":                 "WDJB-MJHT\x1b[2J",
			"verification_uri":          "https://app.latem.dev/connect",
			"verification_uri_complete": "https://app.latem.dev/connect?code=WDJB-MJHT",
			"expires_in":                600,
			"interval":                  cp.interval,
		})
	})
	mux.HandleFunc("POST /claim/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if body["device_code"] != "dc_secret_device_code" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		answer := "authorization_pending"
		if cp.polls < len(cp.answers) {
			answer = cp.answers[cp.polls]
		}
		cp.polls++
		if answer == "unavailable" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
			return
		}
		if answer == "token" {
			writeJSON(w, http.StatusOK, map[string]string{"machine_id": cp.id, "token": cp.token})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": answer})
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.bearer = r.Header.Get("Authorization")
		cp.mu.Unlock()
		if cp.bearer != "Bearer "+cp.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = conn.Write(r.Context(), websocket.MessageText,
			[]byte(`{"kind":"hello","protocol_version":"1.0","hello":{"machine_id":"`+cp.id+`"}}`))
		_, _, _ = conn.Read(r.Context())
	})
	cp.srv = httptest.NewServer(mux)
	t.Cleanup(cp.srv.Close)
	return cp
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (cp *controlPlane) agentURL() string { return wsURL(cp.srv.URL) + "/agent" }

// noSleep replaces the poll interval with a record of it.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	var mu sync.Mutex
	prev := claimSleep
	claimSleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { claimSleep = prev })
	return &slept
}

// The whole flow, against the contract: a code is asked for with the
// machine's name, OS, arch, labels and version; the poll waits through a
// pending and a slow_down; the token arrives once; and the rest of
// registration runs exactly as it does for a pasted token.
func TestRegister_DeviceFlowEndToEnd(t *testing.T) {
	cp := newControlPlane(t, "authorization_pending", "slow_down", "authorization_pending", "token")
	slept := noSleep(t)
	dir := configDirWith(t, baseConfig)

	out, err := registerIn(t, dir, "", "--url", cp.agentURL(), "--name", "mac-mini")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	// The /claim body, field by field as the contract names them.
	if len(cp.claims) != 1 {
		t.Fatalf("claims = %d", len(cp.claims))
	}
	claim := cp.claims[0]
	keys := make([]string, 0, len(claim))
	for k := range claim {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"agent_version", "arch", "labels", "name", "os"}; !slices.Equal(keys, want) {
		t.Errorf("/claim fields = %v, want exactly %v", keys, want)
	}
	if claim["name"] != "mac-mini" || claim["os"] != runtime.GOOS || claim["arch"] != runtime.GOARCH || claim["agent_version"] != version {
		t.Errorf("/claim = %v", claim)
	}
	if labels, _ := claim["labels"].([]any); len(labels) != 2 || labels[0] != "linux" || labels[1] != "docker" {
		t.Errorf("labels = %v, want the runners' labels", claim["labels"])
	}

	// interval 2s, then +5s after slow_down, kept for the polls after it.
	if want := []time.Duration{2 * time.Second, 2 * time.Second, 7 * time.Second, 7 * time.Second}; !slices.Equal(*slept, want) {
		t.Errorf("waits = %v, want %v", *slept, want)
	}

	// The token the claim minted is the one the handshake carried, and the
	// one on disk.
	if cp.bearer != "Bearer "+cp.token {
		t.Errorf("handshake authorization = %q", cp.bearer)
	}
	key, err := os.ReadFile(filepath.Join(dir, "agent.key"))
	if err != nil || strings.TrimSpace(string(key)) != cp.token {
		t.Errorf("agent.key = %q, %v", key, err)
	}
	cfg := loadConfigIn(t, dir)
	if cfg.Machine.ID != "mch_claimed" || cfg.Broker.Remote.URL != cp.agentURL() {
		t.Errorf("machine = %+v, remote = %+v", cfg.Machine, cfg.Broker.Remote)
	}

	for _, want := range []string{"https://app.latem.dev/connect?code=WDJB-MJHT", "approve the code  WDJB-MJHT", "approved", "registered."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{cp.token, "dc_secret_device_code", "\x1b"} {
		if strings.Contains(out, never) {
			t.Errorf("output carries %q:\n%s", never, out)
		}
	}
}

func TestRegister_DeviceFlowEndsWithNothingWritten(t *testing.T) {
	for answer, want := range map[string]string{
		"access_denied": "declined in the browser",
		"expired_token": "expired, or was already used",
		"server_error":  "refused the claim: server_error",
	} {
		t.Run(answer, func(t *testing.T) {
			cp := newControlPlane(t, "authorization_pending", answer)
			noSleep(t)
			dir := configDirWith(t, baseConfig)

			_, err := registerIn(t, dir, "", "--url", cp.agentURL())
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was written") {
				t.Fatalf("want %q and nothing written, got %v", want, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "agent.key")); err == nil {
				t.Error("a token file was written")
			}
			if readConfigFile(t, dir) != baseConfig {
				t.Error("config.toml was changed")
			}
		})
	}
}

// latem-cloud answers 503 while its database is unreachable; the wait rides
// it out rather than ending the claim, and gives up only after several in a
// row.
func TestClaimWait_RidesOutAnUnavailableControlPlane(t *testing.T) {
	cp := newControlPlane(t, "unavailable", "unavailable", "token")
	c := newClaimClient(cp.srv.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	tok, err := c.wait(context.Background(), claimStart{DeviceCode: "dc_secret_device_code", Interval: 1})
	if err != nil || tok.Token != cp.token {
		t.Fatalf("got %+v, %v", tok, err)
	}

	down := newControlPlane(t, "unavailable", "unavailable", "unavailable", "unavailable", "unavailable", "token")
	c = newClaimClient(down.srv.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := c.wait(context.Background(), claimStart{DeviceCode: "dc_secret_device_code", Interval: 1}); err == nil ||
		!strings.Contains(err.Error(), "temporarily_unavailable") {
		t.Errorf("five 503s in a row: %v", err)
	}
}

// latem-cloud's answers outside the contract's happy path (its protocol.md §1
// "Registering"): a 503 while its database is unreachable is retried; a 429
// from the per-network limit says when to come back; an invalid_request says
// why, from its error_description.
func TestClaimStart_LatemCloudsEdgeCases(t *testing.T) {
	answer := func(status int, header map[string]string, body string, after int) *httptest.Server {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			calls++
			if calls > after {
				writeJSON(w, http.StatusOK, map[string]any{"device_code": "d", "user_code": "BCDF-GHJK",
					"verification_uri": "https://app.latem.dev/connect", "expires_in": 600, "interval": 5})
				return
			}
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	client := func(srv *httptest.Server) *claimClient {
		c := newClaimClient(srv.URL)
		c.sleep = func(context.Context, time.Duration) error { return nil }
		return c
	}
	req := newClaimRequest("m", nil, "dev")

	if st, err := client(answer(503, nil, `{"error":"temporarily_unavailable"}`, 2)).start(context.Background(), req); err != nil ||
		st.UserCode != "BCDF-GHJK" {
		t.Errorf("two 503s then a claim: %+v, %v", st, err)
	}
	if _, err := client(answer(503, nil, `{"error":"temporarily_unavailable"}`, 99)).start(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "temporarily_unavailable") {
		t.Errorf("a control plane that stays unavailable: %v", err)
	}
	_, err := client(answer(429, map[string]string{"Retry-After": "600"},
		`{"error":"slow_down","error_description":"too many claims from this network"}`, 99)).start(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "try again in 10m0s") || !strings.Contains(err.Error(), "too many claims") {
		t.Errorf("the per-network limit: %v", err)
	}
	_, err = client(answer(400, nil, `{"error":"invalid_request","error_description":"name is required"}`, 99)).start(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "invalid_request: name is required") {
		t.Errorf("a refused request: %v", err)
	}
}

// What a claim carries is fitted to latem-cloud's limits rather than refused
// over them: name ≤ 64 and never empty, os/arch/version ≤ 32, ≤ 32 labels of
// ≤ 64, no control characters.
func TestNewClaimRequest_FitsTheControlPlanesLimits(t *testing.T) {
	many := make([]string, 40)
	for i := range many {
		many[i] = "label-" + strings.Repeat("x", i)
	}
	req := newClaimRequest("", append([]string{"bad\x1b[31mlabel", strings.Repeat("l", 80)}, many...),
		"v0.2.0-rc.1-1234-gabcdef0123-dirty-and-then-some")
	if req.Name != "latem" {
		t.Errorf("an empty name became %q, want latem", req.Name)
	}
	if len(req.AgentVersion) > 32 || len(req.Labels) != 32 {
		t.Errorf("version %q (%d), %d labels", req.AgentVersion, len(req.AgentVersion), len(req.Labels))
	}
	for _, l := range req.Labels {
		if len(l) > 64 || strings.ContainsRune(l, 0x1b) {
			t.Errorf("label %q", l)
		}
	}
	if got := newClaimRequest(strings.Repeat("n", 70), nil, "dev"); len(got.Name) != 64 || got.Labels == nil {
		t.Errorf("name %d bytes, labels %v", len(got.Name), got.Labels)
	}
	// A cut never splits a character, and a blank label is not sent.
	got := newClaimRequest(strings.Repeat("n", 63)+"é", []string{"  ", "linux"}, "dev")
	if got.Name != strings.Repeat("n", 63) || !utf8.ValidString(got.Name) {
		t.Errorf("name %q split a rune", got.Name)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "linux" {
		t.Errorf("labels = %q, want the blank one dropped", got.Labels)
	}
	if got := newClaimRequest("   ", nil, "dev"); got.Name != "latem" {
		t.Errorf("a blank name became %q", got.Name)
	}
}

// The control plane's deadline is the poll's: past expires_in, waiting longer
// cannot help.
func TestClaimWait_StopsAtTheExpiry(t *testing.T) {
	cp := newControlPlane(t) // pending forever
	now := time.Unix(1_000_000, 0)
	c := newClaimClient(cp.srv.URL)
	c.sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	c.now = func() time.Time { return now }
	_, err := c.wait(context.Background(), claimStart{DeviceCode: "dc_secret_device_code", ExpiresIn: 30, Interval: 5})
	if !errors.Is(err, errClaimExpired) {
		t.Fatalf("got %v, want errClaimExpired", err)
	}
	if cp.polls != 5 {
		t.Errorf("polled %d times in 30s at 5s, want 5", cp.polls)
	}
}

func TestClaimBase(t *testing.T) {
	for in, want := range map[string]string{
		"wss://gw.latem.dev/agent":         "https://gw.latem.dev",
		"wss://gw.latem.dev/agent/":        "https://gw.latem.dev",
		"wss://cp.example.com/latem/agent": "https://cp.example.com/latem",
		"wss://cp.example.com:8443/agent":  "https://cp.example.com:8443",
		"ws://127.0.0.1:8080/agent":        "http://127.0.0.1:8080",
		"ws://localhost/agent":             "http://localhost",
	} {
		if got, err := claimBase(in); err != nil || got != want {
			t.Errorf("claimBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := claimBase("ws://cp.example.com/agent"); err == nil {
		t.Error("a plaintext claim to a remote host was allowed")
	}
}

func TestApprovalLink_OnlyAWebPage(t *testing.T) {
	cases := []struct {
		st   claimStart
		want string
	}{
		{claimStart{VerificationURI: "https://a/connect", VerificationURIComplete: "https://a/connect?code=X"}, "https://a/connect?code=X"},
		{claimStart{VerificationURI: "https://a/connect", VerificationURIComplete: "javascript:alert(1)"}, "https://a/connect"},
		{claimStart{VerificationURI: "file:///etc/passwd"}, ""},
		{claimStart{VerificationURI: "http://evil.example/connect"}, ""},
	}
	for _, tc := range cases {
		if got := approvalLink(tc.st); got != tc.want {
			t.Errorf("approvalLink(%+v) = %q, want %q", tc.st, got, tc.want)
		}
	}
}

// A redirect is the control plane saying it is somewhere else, and the device
// code is not sent after it.
func TestClaimStart_DoesNotFollowARedirect(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, target.URL+"/claim", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	_, err := newClaimClient(srv.URL).start(context.Background(), claimRequest{Name: "m"})
	if err == nil || followed {
		t.Errorf("err = %v, followed = %v", err, followed)
	}
}
