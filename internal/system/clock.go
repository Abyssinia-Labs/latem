package system

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Skew is a comparison between this machine's clock and a server's.
type Skew struct {
	// URL is what was asked.
	URL string
	// Remote is the server's clock, from the HTTP Date header.
	Remote time.Time
	// Local is this machine's clock at the same moment.
	Local time.Time
	// Delta is Local minus Remote. Positive means this machine is ahead.
	Delta time.Duration
	// Measured is false when no Date header came back, in which case Delta is
	// meaningless rather than zero.
	Measured bool
}

// Abs is the magnitude of the skew.
func (s Skew) Abs() time.Duration {
	if s.Delta < 0 {
		return -s.Delta
	}
	return s.Delta
}

// ClockSkew measures this machine's clock against the broker's.
//
// RUNNER_SPEC §19 asks for it because of how the failure presents: a machine
// token is validated against a signing window, and a clock a few minutes out
// makes every connection fail authentication. The agent reports "unauthorized"
// and the operator checks the token, regenerates it, checks it again, and none
// of that is the problem.
//
// The measurement uses the HTTP Date header of a HEAD request, which every
// HTTP server sends and which needs no cooperation from the control plane. The
// broker URL is a WebSocket URL, so the scheme is rewritten: wss to https, ws
// to http.
//
// Two samples are taken and the smaller magnitude wins. Both sources of error
// here only ever inflate the reading: a response served from a cache carries
// the Date it was cached at, and the round trip lands inside the measurement.
// Against api.github.com the first request of a process is reproducibly served
// from cache and reports about thirty seconds of skew on a machine whose clock
// is correct, which would be a false alarm on a check whose warning threshold
// is one minute.
func (s *System) ClockSkew(ctx context.Context, brokerURL string) (Skew, error) {
	probe, err := httpProbeURL(brokerURL)
	if err != nil {
		return Skew{URL: brokerURL}, err
	}

	var best Skew
	var lastErr error
	for i := 0; i < clockSamples; i++ {
		sk, err := s.sampleClock(ctx, probe)
		if err != nil {
			lastErr = err
			continue
		}
		if !best.Measured || sk.Abs() < best.Abs() {
			best = sk
		}
	}
	if !best.Measured {
		return Skew{URL: probe}, lastErr
	}
	return best, nil
}

// clockSamples is how many readings [System.ClockSkew] takes. Two is enough:
// the contaminated reading is the cached one, and a cache answers at most the
// first request.
const clockSamples = 2

func (s *System) sampleClock(ctx context.Context, probe string) (Skew, error) {
	sk := Skew{URL: probe}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probe, nil)
	if err != nil {
		return sk, err
	}
	req.Header.Set("User-Agent", "latem-doctor")
	// Without this a CDN can answer from cache, and a cached response carries
	// the Date it was cached at. Measuring against that reports skew that is
	// not there — observed as an 11-second "drift" against a correctly
	// synchronised clock.
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := s.http().Do(req)
	if err != nil {
		return sk, err
	}
	defer func() { _ = resp.Body.Close() }()

	// The local reading is taken after the response arrives, so the round trip
	// is inside the measurement rather than beside it. That biases the answer
	// by at most one round trip, which is irrelevant next to the minutes of
	// skew this check exists to find.
	sk.Local = time.Now()
	date := resp.Header.Get("Date")
	if date == "" {
		return sk, fmt.Errorf("system: %s returned no Date header, so clock skew cannot be measured", probe)
	}
	remote, err := http.ParseTime(date)
	if err != nil {
		return sk, fmt.Errorf("system: unparsable Date header %q from %s: %w", date, probe, err)
	}
	sk.Remote = remote
	sk.Delta = sk.Local.Sub(remote)
	sk.Measured = true
	return sk, nil
}

func (s *System) http() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return &http.Client{Timeout: s.timeout}
}

// httpProbeURL turns a broker URL into something a HEAD request can reach.
func httpProbeURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("system: no broker URL to measure a clock against")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("system: unparsable broker URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "http", "https":
	default:
		return "", fmt.Errorf("system: broker URL %q has scheme %q, expected ws or wss", raw, u.Scheme)
	}
	return u.String(), nil
}
