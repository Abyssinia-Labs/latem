package mirrorlock

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func needFlock(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the mirror lock is a flock on Linux and macOS only")
	}
}

func TestPath_IsADotNameBesideTheMirror(t *testing.T) {
	if got, want := Path("/m/mirrors/myapp.git/"), "/m/mirrors/.myapp.git.lock"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

// Clones share; a write excludes them and they exclude it. Every hold here is
// its own open in one process, which is the case flock makes work and an
// fcntl lock would not: the agent's goroutines meet each other through it as
// they meet `latem cache prune`.
func TestTryLock_SharedAndExclusive(t *testing.T) {
	needFlock(t)
	mirror := filepath.Join(t.TempDir(), "myapp.git")

	r1, ok, err := TryLock(mirror, false)
	if err != nil || !ok {
		t.Fatalf("first shared hold = %v, %v", ok, err)
	}
	r2, ok, err := TryLock(mirror, false)
	if err != nil || !ok {
		t.Fatalf("second shared hold = %v, %v: clones would queue behind each other", ok, err)
	}
	if _, ok, err := TryLock(mirror, true); err != nil || ok {
		t.Fatalf("exclusive while shared = %v, %v: a gc could delete packs under a clone", ok, err)
	}
	r1()
	r2()

	w, ok, err := TryLock(mirror, true)
	if err != nil || !ok {
		t.Fatalf("exclusive once released = %v, %v", ok, err)
	}
	if _, ok, err := TryLock(mirror, false); err != nil || ok {
		t.Fatalf("shared while exclusive = %v, %v: a clone could list packs a gc is deleting", ok, err)
	}
	w()
}

func TestLock_WaitsForTheHolderAndStopsWithItsContext(t *testing.T) {
	needFlock(t)
	mirror := filepath.Join(t.TempDir(), "myapp.git")

	w, err := Lock(context.Background(), mirror, true)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, mirror, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock under a held write = %v, want the context's deadline", err)
	}

	got := make(chan error, 1)
	go func() {
		r, err := Lock(context.Background(), mirror, false)
		if err == nil {
			r()
		}
		got <- err
	}()
	time.Sleep(3 * poll)
	w()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a shared Lock did not get the lock once the write released it")
	}
}
