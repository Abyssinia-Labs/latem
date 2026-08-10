package remote

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// Backoff is RUNNER_SPEC §7.4's reconnect schedule: 1s → 2s → 4s … capped at
// 60s, with ±20% jitter.
//
// The jitter is not decoration. A control plane that restarts drops every agent
// it has at the same instant, and an undithered schedule brings all of them
// back at the same instant too — a thundering herd on a service that is by
// definition still coming up. Spreading each delay over a 40% window turns a
// spike into a ramp, and it costs nothing.
//
// The zero value is not usable; call [Backoff.orDefault] or use
// [DefaultBackoff].
type Backoff struct {
	// Base is the first delay.
	Base time.Duration
	// Max caps it. The cap matters more than the growth rate: an agent that
	// backed off to an hour would still be silent long after the network came
	// back, and the machine would look dead to everyone including its owner.
	Max time.Duration
	// Jitter is the fraction of the delay to dither by, in each direction.
	// 0.2 is §7.4's ±20%.
	Jitter float64
}

// DefaultBackoff is §7.4 exactly.
var DefaultBackoff = Backoff{Base: time.Second, Max: 60 * time.Second, Jitter: 0.2}

func (b Backoff) orDefault() Backoff {
	if b.Base <= 0 {
		b.Base = DefaultBackoff.Base
	}
	if b.Max <= 0 {
		b.Max = DefaultBackoff.Max
	}
	if b.Jitter < 0 {
		b.Jitter = 0
	}
	if b.Jitter > 1 {
		b.Jitter = 1
	}
	return b
}

// Delay returns the wait before attempt n, counting from zero, dithered by
// rnd — a value in [0,1).
//
// The doubling is computed with a shift rather than math.Pow, and the exponent
// is clamped before the shift rather than the product after it. A 63-bit shift
// on an int64 is undefined-adjacent territory and an agent that has been
// retrying for a day would reach it; clamping first means the sequence
// saturates at Max and stays there rather than wrapping to something absurd.
func (b Backoff) Delay(attempt int, rnd float64) time.Duration {
	b = b.orDefault()
	if attempt < 0 {
		attempt = 0
	}

	d := b.Max
	if attempt < 62 {
		if scaled := b.Base << uint(attempt); scaled > 0 && scaled < b.Max {
			d = scaled
		}
	}

	if b.Jitter == 0 {
		return d
	}
	// rnd in [0,1) maps to a factor in [1-J, 1+J).
	factor := 1 + b.Jitter*(2*rnd-1)
	dithered := time.Duration(math.Round(float64(d) * factor))
	if dithered < 0 {
		return 0
	}
	return dithered
}

// realSleep is the production clock behind [Options.sleep]. It reports false if
// the wait was cut short, which is how Close and a cancelled context stop the
// reconnect loop mid-backoff instead of after a minute.
func realSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func randomJitter() float64 { return rand.Float64() } //nolint:gosec // dithering a reconnect delay, not a secret
