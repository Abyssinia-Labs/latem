package run

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A cancel mid-run terminates within the grace and still collects artifacts —
// the acceptance criterion for WS-10 and the whole reason collecting always
// runs.
func TestCancel_MidRunTerminatesAndStillCollects(t *testing.T) {
	h := newHarness(t)

	running := make(chan struct{})
	var once sync.Once
	h.env.exec = func(ctx context.Context, _ executor.Step, stdout, _ io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "started\n")
		once.Do(func() { close(running) })
		// A well-behaved executor: SIGTERM, wait, SIGKILL, return.
		<-ctx.Done()
		return -1, ctx.Err()
	}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, "out/partial.txt", "half a build")

	done := make(chan Result, 1)
	go func() {
		res, err := o.Execute(t.Context(), r)
		if err != nil {
			t.Errorf("Execute: %v", err)
		}
		done <- res
	}()

	<-running
	if err := o.Cancel(t.Context(), protocol.CancelRequest{
		ID: req.ID, Actor: "phone", Reason: "wrong branch",
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	var res Result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not stop within the grace")
	}

	if res.State != protocol.StateCancelled {
		t.Fatalf("state = %s (%s), want cancelled", res.State, res.ErrorMsg)
	}
	if res.ErrorCode != "" {
		t.Errorf("a cancelled run carries an error code: %s", res.ErrorCode)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("a cancelled run lost its artifacts: %+v", res.Artifacts)
	}

	row := mustGet(t, h.db, req.ID)
	if row.State != protocol.StateCancelled {
		t.Errorf("stored state = %s", row.State)
	}
	if !strings.Contains(row.CancelReason, "wrong branch") || !strings.Contains(row.CancelReason, "phone") {
		t.Errorf("cancel reason = %q, want the reason and the actor", row.CancelReason)
	}

	waitFor(t, "collecting reported", func() bool {
		return slices.Contains(h.broker.states(req.ID), protocol.StateCollecting)
	})
	if h.env.closeCount() != 1 {
		t.Error("the environment leaked on the cancellation path")
	}
	if _, err := os.Stat(filepath.Join(h.source.root, req.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the workspace leaked on the cancellation path")
	}
}

// An executor that ignores the contract cannot hold the run open forever. The
// step is abandoned after the grace, and collecting still happens.
func TestCancel_AbandonsAStepThatWillNotStop(t *testing.T) {
	h := newHarness(t)
	h.opts.CancelGrace = 50 * time.Millisecond

	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.env.exec = func(_ context.Context, _ executor.Step, stdout, _ io.Writer) (int, error) {
		once.Do(func() { close(running) })
		<-release
		// The abandoned goroutine writes into a pipe nobody is draining. It
		// must not block; abandon() closed the read side for exactly this.
		_, _ = io.WriteString(stdout, "too late\n")
		return 0, nil
	}
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	done := make(chan Result, 1)
	go func() {
		res, _ := o.Execute(t.Context(), r)
		done <- res
	}()

	<-running
	_ = o.Cancel(t.Context(), protocol.CancelRequest{ID: "run1"})

	var res Result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the run was not abandoned after the grace")
	}
	close(release)

	if res.State != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled", res.State)
	}
	if !strings.Contains(h.logContents("run1"), "abandoning it") {
		t.Error("the abandonment was not narrated into the log")
	}
}

// §11: a cancel arriving during collecting is ignored. The run finishes as
// whatever it already was.
func TestCancel_DuringCollectingIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.env.exec = func(context.Context, executor.Step, io.Writer, io.Writer) (int, error) {
		return 0, nil
	}
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	runCtx, hnd, err := o.begin(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer o.end(hnd)

	// Simulate having reached collecting, then cancel.
	hnd.enterCollecting()
	if hnd.request("too late", "phone") {
		t.Fatal("a cancel took effect during collecting")
	}
	if runCtx.Err() != nil {
		t.Fatal("the run context was cancelled during collecting")
	}
	if requested, _, _ := hnd.cancelled(); requested {
		t.Fatal("the ignored cancel was still recorded")
	}
}

func TestCancel_WaitingRunIsImmediate(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		want protocol.RunState
	}{
		{"queued", "main", protocol.StateQueued},
		{"awaiting_approval", "feature/x", protocol.StateAwaitingApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			o := h.build()

			req := h.request("test")
			req.Ref = tc.ref
			r, err := o.Admit(t.Context(), req, 0)
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if r.State != tc.want {
				t.Fatalf("precondition: state = %s, want %s", r.State, tc.want)
			}

			if err := o.Cancel(t.Context(), protocol.CancelRequest{
				ID: req.ID, Reason: "changed my mind",
			}); err != nil {
				t.Fatalf("Cancel: %v", err)
			}

			row := mustGet(t, h.db, req.ID)
			if row.State != protocol.StateCancelled {
				t.Fatalf("state = %s, want cancelled", row.State)
			}
			if row.FinishedAt == nil {
				t.Error("cancelled without a finish time")
			}
			if !strings.Contains(row.CancelReason, "changed my mind") {
				t.Errorf("cancel reason = %q", row.CancelReason)
			}
			if row.ExpiresAt != nil {
				t.Error("a terminal run kept its TTL deadline")
			}
		})
	}
}

func TestCancel_ClaimedRunIsImmediate(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	h.admitAndClaim(o, h.request("test"))
	if err := o.Cancel(t.Context(), protocol.CancelRequest{ID: "run1"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := mustGet(t, h.db, "run1").State; got != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled", got)
	}
}

// A run cancelled before Execute picks it up cannot start: claimed → preparing
// is not an edge out of cancelled, so the store refuses it.
func TestCancel_BeatsExecuteToTheClaim(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	if err := o.Cancel(t.Context(), protocol.CancelRequest{ID: "run1"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled", res.State)
	}
	if h.exec.prepared != 0 || len(h.source.fetched) != 0 {
		t.Error("a cancelled run still did work")
	}
}

func TestCancel_IsANoOpForUnknownTerminalAndMalformed(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	// Unknown.
	if err := o.Cancel(t.Context(), protocol.CancelRequest{ID: "never-existed"}); err != nil {
		t.Errorf("unknown run: %v", err)
	}
	// Malformed — an id that could name no run and could name a path.
	if err := o.Cancel(t.Context(), protocol.CancelRequest{ID: "../../etc/passwd"}); err != nil {
		t.Errorf("malformed id: %v", err)
	}
	// Terminal, and then again: a phone tapping Stop twice.
	h.run(o, h.request("test"))
	for i := range 2 {
		if err := o.Cancel(t.Context(), protocol.CancelRequest{ID: "run1"}); err != nil {
			t.Errorf("terminal cancel %d: %v", i, err)
		}
	}
	if got := mustGet(t, h.db, "run1").State; got != protocol.StateSucceeded {
		t.Fatalf("a terminal run moved on cancel: %s", got)
	}
}

func TestCancel_IsIdempotentMidRun(t *testing.T) {
	h := newHarness(t)
	running := make(chan struct{})
	var once sync.Once
	h.env.exec = func(ctx context.Context, _ executor.Step, _, _ io.Writer) (int, error) {
		once.Do(func() { close(running) })
		<-ctx.Done()
		return -1, ctx.Err()
	}
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	done := make(chan Result, 1)
	go func() {
		res, _ := o.Execute(t.Context(), r)
		done <- res
	}()

	<-running
	for range 3 {
		if err := o.Cancel(t.Context(), protocol.CancelRequest{ID: "run1", Reason: "first"}); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	}
	res := <-done
	if res.State != protocol.StateCancelled {
		t.Fatalf("state = %s", res.State)
	}
	// Only the first reason is kept; a second Stop does not rewrite history.
	if !strings.Contains(mustGet(t, h.db, "run1").CancelReason, "first") {
		t.Error("the first cancel reason was lost")
	}
}

func TestLiveAndTail(t *testing.T) {
	h := newHarness(t)
	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.env.exec = func(_ context.Context, _ executor.Step, stdout, _ io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "hello from the build\n")
		once.Do(func() { close(running) })
		<-release
		return 0, nil
	}
	o := h.build()

	if o.IsLive("run1") || len(o.Live()) != 0 {
		t.Fatal("a run is live before it started")
	}

	r := h.admitAndClaim(o, h.request("test"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.Execute(t.Context(), r)
	}()

	<-running
	if !o.IsLive("run1") || !slices.Contains(o.Live(), "run1") {
		t.Error("the run is not reported live")
	}

	ch, stop, ok := o.Subscribe("run1", 8)
	if !ok {
		t.Fatal("could not subscribe to a live run")
	}
	defer stop()

	waitFor(t, "the build's line in the ring", func() bool {
		frames, found := o.Tail("run1")
		if !found {
			return false
		}
		for _, f := range frames {
			if strings.Contains(f.Line, "hello from the build") {
				return true
			}
		}
		return false
	})

	close(release)
	<-done

	if o.IsLive("run1") {
		t.Error("the run is still live after it finished")
	}
	if _, _, ok := o.Subscribe("run1", 1); ok {
		t.Error("a finished run is still subscribable")
	}
	if _, ok := o.Tail("run1"); ok {
		t.Error("a finished run still has a ring")
	}
	_ = ch
}

func TestCancelReason_Rendering(t *testing.T) {
	for _, tc := range []struct {
		req  protocol.CancelRequest
		want string
	}{
		{protocol.CancelRequest{}, "cancelled"},
		{protocol.CancelRequest{Actor: "ops"}, "cancelled by ops"},
		{protocol.CancelRequest{Reason: "wrong sha"}, "wrong sha"},
		{protocol.CancelRequest{Reason: "wrong sha", Actor: "ops"}, "wrong sha (by ops)"},
	} {
		if got := cancelReason(tc.req); got != tc.want {
			t.Errorf("cancelReason(%+v) = %q, want %q", tc.req, got, tc.want)
		}
	}
}
