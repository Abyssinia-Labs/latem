package remote

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestBackoff_MatchesTheSpecSchedule pins §7.4: 1s → 2s → 4s … capped at 60s.
//
// The cap is the expensive half to get wrong. Unbounded doubling would leave an
// agent silent for hours after a five-minute outage, and a machine that has been
// back for an hour but has not reconnected looks dead to everyone including its
// owner.
func TestBackoff_MatchesTheSpecSchedule(t *testing.T) {
	// rnd = 0.5 is the midpoint of the jitter window, so it dithers by nothing
	// and the bare schedule is visible.
	const mid = 0.5

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
		60 * time.Second,
	}
	for i, w := range want {
		if got := DefaultBackoff.Delay(i, mid); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i, got, w)
		}
	}
}

// TestBackoff_JitterStaysInsideTwentyPercent.
//
// The dither is not decoration. A control plane that restarts drops every agent
// it has at the same instant; an undithered schedule brings all of them back at
// the same instant too, onto a service that is by definition still coming up.
// It also must not exceed the window, or the cap stops meaning what §7.4 says.
func TestBackoff_JitterStaysInsideTwentyPercent(t *testing.T) {
	tests := map[string]struct {
		rnd  float64
		want time.Duration
	}{
		"lower bound": {0.0, 800 * time.Millisecond},
		"midpoint":    {0.5, time.Second},
		"upper edge":  {0.9999999, 1200 * time.Millisecond},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := DefaultBackoff.Delay(0, tc.rnd)
			if d := time.Duration(math.Abs(float64(got - tc.want))); d > time.Millisecond {
				t.Errorf("Delay(0, %v) = %s, want ~%s", tc.rnd, got, tc.want)
			}
		})
	}

	// And at the cap, so a dithered 60s never becomes 72s.
	for _, rnd := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
		got := DefaultBackoff.Delay(20, rnd)
		if got < 48*time.Second || got > 72*time.Second {
			t.Errorf("Delay(20, %v) = %s, outside 60s ±20%%", rnd, got)
		}
	}
}

// TestBackoff_DoesNotOverflowAfterADayOfRetrying.
//
// The exponent is clamped before the shift rather than the product after it.
// A machine whose control plane has been gone for a day reaches attempt 1,400,
// and `1s << 1400` on an int64 is not 60 seconds — it is whatever the hardware
// felt like, including a negative duration, which would turn the reconnect loop
// into a spin.
func TestBackoff_DoesNotOverflowAfterADayOfRetrying(t *testing.T) {
	for _, attempt := range []int{-1, 0, 61, 62, 63, 64, 1000, math.MaxInt32} {
		got := DefaultBackoff.Delay(attempt, 0.5)
		if got <= 0 || got > 60*time.Second {
			t.Errorf("Delay(%d) = %s; it must saturate at the cap and stay positive", attempt, got)
		}
	}
}

// TestBackoff_ZeroValueIsUsable. Options.Backoff is a struct, so the zero value
// is what a caller who did not think about it gets, and it has to be the spec's
// schedule rather than a busy loop.
func TestBackoff_ZeroValueIsUsable(t *testing.T) {
	var zero Backoff
	if got := zero.Delay(0, 0.5); got != time.Second {
		t.Errorf("the zero Backoff produced %s for the first attempt, want 1s", got)
	}
	if got := zero.Delay(20, 0.5); got != 60*time.Second {
		t.Errorf("the zero Backoff produced %s at the cap, want 60s", got)
	}
}

// TestRealSleep_IsInterruptible. Close must not have to wait out a sixty-second
// backoff, or shutting an agent down would take a minute for no reason.
func TestRealSleep_IsInterruptible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- realSleep(ctx, time.Minute) }()

	cancel()
	select {
	case completed := <-done:
		if completed {
			t.Error("realSleep reported a completed wait after its context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("realSleep ignored its cancelled context")
	}
}
