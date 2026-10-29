package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
)

// Device-code registration (A54), the RFC 8628 shape: the machine asks the
// control plane for a code, a person approves it in the browser where they
// are already signed in, and the machine collects its token. Nothing is
// copied between the two by hand, and the token is shown to nobody.
//
// The endpoints are the control plane's, under the HTTPS origin of its agent
// URL:
//
//	POST <base>/claim        {"name","os","arch","labels","agent_version"}
//	  200 {"device_code","user_code","verification_uri",
//	       "verification_uri_complete","expires_in","interval"}
//	POST <base>/claim/token  {"device_code"}
//	  200 {"machine_id","token"}                  — once; the claim is then spent
//	  400 {"error":"authorization_pending"|"slow_down"|"expired_token"|"access_denied"}

// defaultAgentURL is the hosted control plane's agent endpoint.
const defaultAgentURL = "wss://gw.latem.dev/agent"

// registerURL is --url's default: defaultAgentURL, except under test, where
// a command line that forgot --url must not reach the production control
// plane.
var registerURL = defaultAgentURL

// maxClaimResponseBytes bounds what the control plane may send back. Every
// legal answer is a few hundred bytes.
const maxClaimResponseBytes = 64 << 10

// The defaults RFC 8628 gives a device that is told nothing: poll every five
// seconds, add five more on slow_down. The expiry is the control plane's ten
// minutes, assumed when it does not say.
const (
	defaultClaimInterval = 5 * time.Second
	slowDownStep         = 5 * time.Second
	defaultClaimExpiry   = 10 * time.Minute
	// maxClaimNetworkErrors is how many polls in a row may fail to reach the
	// control plane before the wait is abandoned. A laptop changing networks
	// mid-approval should not cost the person their code.
	maxClaimNetworkErrors = 5
)

type claimRequest struct {
	Name         string   `json:"name"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Labels       []string `json:"labels"`
	AgentVersion string   `json:"agent_version"`
}

type claimStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type claimToken struct {
	MachineID string `json:"machine_id"`
	Token     string `json:"token"`
}

type claimError struct {
	Error string `json:"error"`
}

// claimClient speaks the two endpoints.
type claimClient struct {
	base string
	http *http.Client
	// sleep waits between polls. A seam: a test of slow_down should not take
	// ten seconds.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

func newClaimClient(base string) *claimClient {
	return &claimClient{
		base: base,
		http: &http.Client{
			Timeout: 30 * time.Second,
			// Not followed. The body is a device code, and a redirect is the
			// control plane's endpoint saying it is somewhere else — which
			// the person who typed --url should hear about, not have
			// silently honoured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sleep: claimSleep,
		now:   time.Now,
	}
}

// claimSleep is how a claim waits between polls; a test replaces it.
var claimSleep = sleepContext

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// claimBase is the HTTPS origin the claim endpoints live under, from the agent
// URL: wss://gw.latem.dev/agent is https://gw.latem.dev. A trailing /agent
// segment is dropped, and any path before it kept, so a control plane mounted
// under a prefix still works.
//
// Plain http is accepted only for a loopback host, the rule internal/config
// applies to the agent URL itself: a device code and a machine token are not
// things to send in the clear.
func claimBase(agentURL string) (string, error) {
	u, err := neturl.Parse(agentURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("%s is plaintext to a host that is not this machine; registration needs wss://", agentURL)
		}
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("%s: scheme %q is not a control plane's", agentURL, u.Scheme)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/agent")
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return strings.TrimSuffix(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// start asks for a code.
func (c *claimClient) start(ctx context.Context, req claimRequest) (claimStart, error) {
	if req.Labels == nil {
		req.Labels = []string{}
	}
	var st claimStart
	status, body, err := c.post(ctx, "/claim", req)
	if err != nil {
		return st, err
	}
	if status != http.StatusOK {
		return st, fmt.Errorf("%s/claim answered %d: %s", c.base, status, describeBody(body))
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("%s/claim answered something that is not a claim: %w", c.base, err)
	}
	if st.DeviceCode == "" || st.UserCode == "" || st.VerificationURI == "" {
		return st, fmt.Errorf("%s/claim answered without a device code, a user code and a page to approve it on", c.base)
	}
	return st, nil
}

// errClaimDenied and errClaimExpired are the two ends of a claim that are the
// person's doing rather than a fault.
var (
	errClaimDenied  = errors.New("the request was declined in the browser")
	errClaimExpired = errors.New("the code expired before it was approved")
)

// wait polls until the claim is approved, declined or expired, honouring the
// control plane's interval and every slow_down.
func (c *claimClient) wait(ctx context.Context, st claimStart) (claimToken, error) {
	interval := time.Duration(st.Interval) * time.Second
	if interval <= 0 {
		interval = defaultClaimInterval
	}
	expiry := time.Duration(st.ExpiresIn) * time.Second
	if expiry <= 0 {
		expiry = defaultClaimExpiry
	}
	deadline := c.now().Add(expiry)

	failures := 0
	for {
		if err := c.sleep(ctx, interval); err != nil {
			return claimToken{}, err
		}
		if !c.now().Before(deadline) {
			return claimToken{}, errClaimExpired
		}
		status, body, err := c.post(ctx, "/claim/token", map[string]string{"device_code": st.DeviceCode})
		if err == nil && (status == http.StatusTooManyRequests || status >= http.StatusInternalServerError) {
			// The control plane is there and cannot answer yet — latem-cloud
			// says 503 temporarily_unavailable while its database is out of
			// reach. Waiting is the answer, as for a dropped connection.
			if status == http.StatusTooManyRequests {
				interval += slowDownStep
			}
			err = fmt.Errorf("answered %d: %s", status, describeBody(body))
		}
		if err != nil {
			if ctx.Err() != nil {
				return claimToken{}, ctx.Err()
			}
			if failures++; failures >= maxClaimNetworkErrors {
				return claimToken{}, fmt.Errorf("could not reach %s while waiting for approval: %w", c.base, err)
			}
			continue
		}
		failures = 0
		switch status {
		case http.StatusOK:
			var tok claimToken
			if err := json.Unmarshal(body, &tok); err != nil || strings.TrimSpace(tok.Token) == "" {
				return claimToken{}, fmt.Errorf("%s/claim/token approved the claim and sent no token", c.base)
			}
			tok.Token = strings.TrimSpace(tok.Token)
			return tok, nil
		case http.StatusBadRequest:
			var e claimError
			_ = json.Unmarshal(body, &e)
			switch e.Error {
			case "authorization_pending":
			case "slow_down":
				interval += slowDownStep
			case "expired_token":
				return claimToken{}, errClaimExpired
			case "access_denied":
				return claimToken{}, errClaimDenied
			default:
				return claimToken{}, fmt.Errorf("%s/claim/token refused the claim: %s", c.base, describeBody(body))
			}
		default:
			return claimToken{}, fmt.Errorf("%s/claim/token answered %d: %s", c.base, status, describeBody(body))
		}
	}
}

func (c *claimClient) post(ctx context.Context, path string, v any) (int, []byte, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClaimResponseBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > maxClaimResponseBytes {
		return 0, nil, fmt.Errorf("%s%s answered with more than %d bytes", c.base, path, maxClaimResponseBytes)
	}
	return resp.StatusCode, body, nil
}

// describeBody is a response body fit to print: the error field when there is
// one, otherwise the first line, without control characters either way.
func describeBody(body []byte) string {
	var e claimError
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return displayText(e.Error)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	if line == "" {
		return "(no body)"
	}
	return displayText(line)
}

// displayText drops what a terminal would interpret. A user code and a link
// are the control plane's text, printed to a person's terminal, and an escape
// sequence in either could rewrite what that terminal shows.
func displayText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// approvalLink is the page to open: the one carrying the code when the control
// plane gave it, if it is a web page at all.
func approvalLink(st claimStart) string {
	for _, raw := range []string{st.VerificationURIComplete, st.VerificationURI} {
		u, err := neturl.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			continue
		}
		if u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
			return u.String()
		}
	}
	return ""
}

// openBrowser opens link for the person at the keyboard. A seam: a test must
// not open a browser on the machine running it.
var openBrowser = func(link string) error {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux":
		name = "xdg-open"
	default:
		return fmt.Errorf("no browser opener on %s", runtime.GOOS)
	}
	// #nosec G204 -- argv, no shell; link is an https URL approvalLink parsed.
	c := exec.Command(name, link)
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}
