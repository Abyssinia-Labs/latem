package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestVerify_ProvesTheTokenBeforeAnythingIsWritten.
//
// `latem register` writes a token file and edits config.toml, and it does both
// only after this returns. The cases below are the four answers a control plane
// can give, and each has to be distinguishable: a refused token is a different
// conversation with the operator from an unreachable host.
func TestVerify_ProvesTheTokenBeforeAnythingIsWritten(t *testing.T) {
	t.Run("a hello carries the assigned machine id", func(t *testing.T) {
		srv := verifyServer(t, "tok", func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageText,
				[]byte(`{"kind":"hello","hello":{"machine_id":"mch_new","message":"welcome"}}`))
		})

		got, err := Verify(t.Context(), VerifyOptions{URL: srv, Token: "tok", MachineID: "mch_old"})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.MachineID != "mch_new" {
			t.Errorf("MachineID = %q, want the one the control plane assigned", got.MachineID)
		}
		if got.Message != "welcome" {
			t.Errorf("Message = %q", got.Message)
		}
	})

	t.Run("silence is success", func(t *testing.T) {
		// A control plane that accepts the connection and says nothing has
		// proven both things registration needs to know. Requiring a hello would
		// make the command fail against a conformant server with nothing to add.
		srv := verifyServer(t, "tok", nil)

		got, err := Verify(t.Context(), VerifyOptions{
			URL: srv, Token: "tok", MachineID: "mch_keep", Timeout: 300 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.MachineID != "mch_keep" {
			t.Errorf("MachineID = %q; a server that assigns none must leave the existing id alone", got.MachineID)
		}
	})

	t.Run("a refused token is ErrRevoked and not a network failure", func(t *testing.T) {
		srv := verifyServer(t, "right", nil)
		_, err := Verify(t.Context(), VerifyOptions{URL: srv, Token: "wrong"})
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("Verify returned %v, want ErrRevoked — register has to say 'check the token', not 'check the url'", err)
		}
	})

	t.Run("an unreachable host is not ErrRevoked", func(t *testing.T) {
		_, err := Verify(t.Context(), VerifyOptions{URL: "ws://127.0.0.1:1/agent", Token: "tok"})
		if err == nil {
			t.Fatal("Verify succeeded against a port nothing is listening on")
		}
		if errors.Is(err, ErrRevoked) {
			t.Error("an unreachable control plane was reported as a refused token; the operator would go and rotate a perfectly good credential")
		}
	})

	t.Run("a frame that is not a hello is skipped rather than fatal", func(t *testing.T) {
		// A server may reasonably open with a replayed command for a machine
		// that is re-registering. Refusing to register because of it is absurd.
		srv := verifyServer(t, "tok", func(ctx context.Context, conn *websocket.Conn) {
			cmd := protocol.NewRunCommand(protocol.RunRequest{ID: "r_1", Repo: "a", Job: "b", Ref: "c"})
			data, _ := encode(frame{Kind: kindCommand, Command: &cmd})
			_ = conn.Write(ctx, websocket.MessageText, data)
			_ = conn.Write(ctx, websocket.MessageText,
				[]byte(`{"kind":"hello","hello":{"machine_id":"mch_after"}}`))
		})

		got, err := Verify(t.Context(), VerifyOptions{URL: srv, Token: "tok"})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.MachineID != "mch_after" {
			t.Errorf("MachineID = %q; the hello after the command was missed", got.MachineID)
		}
	})

	t.Run("what it will not do without", func(t *testing.T) {
		if _, err := Verify(t.Context(), VerifyOptions{Token: "tok"}); err == nil {
			t.Error("Verify accepted an empty url")
		}
		if _, err := Verify(t.Context(), VerifyOptions{URL: "ws://x/agent"}); err == nil {
			t.Error("Verify accepted an empty token")
		}
	})
}

// verifyServer stands up a control plane that checks the bearer token and then
// runs one scripted exchange.
func verifyServer(t *testing.T, token string, script func(context.Context, *websocket.Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if script != nil {
			script(r.Context(), conn)
		}
		// Read the agent's resume frame so the exchange is complete before the
		// handler returns and the connection goes away.
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
