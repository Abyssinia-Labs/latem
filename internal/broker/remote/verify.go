package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// DefaultVerifyTimeout bounds the whole of [Verify]. `latem register` is
// interactive, and a person waiting at a prompt for a control plane that is not
// answering wants to be told so rather than to keep waiting.
const DefaultVerifyTimeout = 15 * time.Second

// VerifyOptions is one connectivity check with a candidate token.
type VerifyOptions struct {
	// URL is the control plane's agent endpoint.
	URL string
	// Token is the machine token to present. It is passed by value rather than
	// as a path because [Verify] runs before the token has been written: a
	// config naming a control plane that rejected us is worse than no config at
	// all, so registration proves the token first and persists second.
	Token string
	// MachineID is the identifier this machine already has, if any. A machine
	// re-registering says who it was.
	MachineID string
	// Timeout defaults to [DefaultVerifyTimeout].
	Timeout time.Duration
}

// Registration is what the control plane said in reply.
type Registration struct {
	// MachineID is the identifier the control plane assigned, or empty if it
	// did not assign one. An empty value is not a failure: connectivity and the
	// token were still proven, and `[machine] id` stays as it was.
	MachineID string
	// Message is the control plane's own words for the operator, if it sent
	// any. It is display text from outside the trust boundary and is printed
	// verbatim, never interpolated into anything.
	Message string
}

// Verify proves a URL and a token before anything is written to disk.
//
// It performs the handshake, sends the same [protocol.Resume] frame a reconnect
// would — with no runs, because a machine registering owes the control plane
// nothing yet — and waits briefly for the server's hello, which is where a
// machine id comes from.
//
// A 401 or 403 is reported as [ErrRevoked] so `latem register` can say "the
// control plane refused this token" rather than "connection failed".
//
// Silence is success. A control plane that accepts the connection and says
// nothing has proven the only two things registration needs to know: the URL is
// right and the token is accepted. Requiring a hello would make this command
// fail against a conformant server that simply has nothing to add.
func Verify(ctx context.Context, opts VerifyOptions) (Registration, error) {
	switch {
	case opts.URL == "":
		return Registration{}, errors.New("remote: Verify requires a url")
	case opts.Token == "":
		return Registration{}, errors.New("remote: Verify requires a machine token")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultVerifyTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+opts.Token)
	header.Set("Latem-Protocol-Version", protocol.Version)

	// bodyclose is suppressed on the call below, as it is on the identical call
	// in conn.go: websocket.Dial owns this body, sets it to nil on success, and
	// has already closed it on failure.
	conn, resp, err := websocket.Dial(ctx, opts.URL, &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose
	if err != nil {
		switch statusOf(resp) {
		case http.StatusUnauthorized, http.StatusForbidden:
			return Registration{}, fmt.Errorf("%w: %s answered %s", ErrRevoked, opts.URL, resp.Status)
		default:
			return Registration{}, fmt.Errorf("remote: could not reach %s: %w", opts.URL, err)
		}
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(MaxFrameBytes)

	r := protocol.Resume{MachineID: opts.MachineID}
	data, err := encode(frame{Kind: kindResume, Resume: &r})
	if err != nil {
		return Registration{}, err
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return Registration{}, fmt.Errorf("remote: %s accepted the connection but would not take a resume frame: %w", opts.URL, err)
	}

	// One read, bounded. Anything that is not a hello is skipped rather than
	// treated as an error: a server may reasonably open with a replayed command
	// for a machine that is re-registering, and refusing to register because of
	// it would be absurd.
	out := Registration{MachineID: opts.MachineID}
	for {
		typ, msg, rerr := conn.Read(ctx)
		if rerr != nil {
			// A timeout here means the server said nothing, which is success.
			// Any other read failure after a completed handshake is still a
			// verified token; the connection dying a moment later is the
			// reconnect loop's problem, not registration's.
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return out, nil
		}
		if typ != websocket.MessageText {
			continue
		}
		f, derr := decode(msg)
		if derr != nil || f.Kind != kindHello {
			continue
		}
		if f.Hello.MachineID != "" {
			out.MachineID = f.Hello.MachineID
		}
		out.Message = f.Hello.Message
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return out, nil
	}
}
