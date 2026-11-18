package source

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/mirrorlock"
)

// TestConcurrent_SameRepo runs several acquisitions of one repo at once.
//
// Fetches into a bare repository are serialised because concurrent fetch is
// not reliably safe — two of them race on packed-refs and on temporary pack
// names. Everything else here is a read of an immutable object store. Under
// -race this also asserts the Manager's own state (the per-repo mutex map) is
// safe.
func TestConcurrent_SameRepo(t *testing.T) {
	f := newFixture(t)
	f.up.annotatedTag("v1.0", "release one")

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mirror, err := f.mgr.Fetch(context.Background(), f.repo)
			if err != nil {
				errs[i] = err
				return
			}
			ws, err := mirror.Acquire(context.Background(), fmt.Sprintf("run_%d", i), "refs/heads/main")
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = ws.SHA
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Errorf("run %d resolved %s, run 0 resolved %s", i, results[i], results[0])
		}
	}

	names, err := f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != n {
		t.Errorf("got %d workspaces, want %d: %v", len(names), n, names)
	}
}

// TestConcurrent_DifferentRepos proves the mutex is per repo and not global.
// A global lock would be correct and would also make a machine with four
// runners fetch four repos in series.
func TestConcurrent_DifferentRepos(t *testing.T) {
	root := t.TempDir()
	mgr := newManager(t, root)

	const n = 4
	repos := make([]Repo, n)
	for i := range n {
		up := newUpstream(t, filepath.Join(root, fmt.Sprintf("upstream-%d", i)))
		up.commit("id.txt", fmt.Sprintf("repo %d\n", i), "identify")
		repos[i] = Repo{Key: fmt.Sprintf("repo-%d", i), URL: up.dir}
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mirror, err := mgr.Fetch(context.Background(), repos[i])
			if err != nil {
				errs[i] = err
				return
			}
			ws, err := mirror.Acquire(context.Background(), fmt.Sprintf("run_r%d", i), "refs/heads/main")
			if err != nil {
				errs[i] = err
				return
			}
			content := runGit(t, ws.Path, "show", "HEAD:id.txt")
			if want := fmt.Sprintf("repo %d", i); content != want {
				errs[i] = fmt.Errorf("workspace content = %q, want %q", content, want)
			}
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("repo %d: %v", i, err)
		}
	}
}

// TestClonesDoNotQueueBehindEachOther is the property RUNNER_SPEC §12 states
// in one sentence and which nothing else in this file would catch: "Clones out
// of the mirror are reads and need no lock, so parallel runs on the same repo
// do not queue behind each other."
//
// The test holds every clone at a barrier that only releases once all of them
// have arrived. If Checkout took the per-repo mutex — the natural mistake when
// someone later adds a mirror write to that path — the first clone would block
// the others' Fetch, no more than one would ever arrive, and this times out
// rather than passing slowly.
func TestClonesDoNotQueueBehindEachOther(t *testing.T) {
	f := newFixture(t)

	const n = 4
	arrived := make(chan struct{}, n)
	release := make(chan struct{})
	f.mgr.beforeClone = func(string) {
		arrived <- struct{}{}
		<-release
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mirror, err := f.mgr.Fetch(context.Background(), f.repo)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = mirror.Acquire(context.Background(), fmt.Sprintf("run_p%d", i), "refs/heads/main")
		}()
	}

	deadline := time.After(30 * time.Second)
	for i := range n {
		select {
		case <-arrived:
		case <-deadline:
			close(release)
			wg.Wait()
			t.Fatalf("only %d of %d clones were in flight at once: clones are queueing "+
				"behind the per-repo fetch mutex", i, n)
		}
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
}

// TestClonesDoNotOverlapMirrorWrites is the race of 2026-11-18 (A112): runs of
// one repository checking out while the mirror they clone from is being
// written. A writer here is what it is on a machine: the agent's own fetch,
// and a gc that deletes the packs it repacks — from `latem cache prune`, a
// second process, which is why it takes the lock through the file
// ([mirrorlock]) and not through the Manager.
//
// `git clone --local` lists objects/ and hard-links what it listed. A pack or
// a loose object a repack deletes between the two leaves the workspace without
// it, and the clone fails, or the checkout after it says "unable to read
// tree". With Checkout's clone holding the mirror's lock shared, the writes
// wait for it and it waits for them, and no run fails. Take the lock out of
// [Manager.cloneLocal] and this fails within a few rounds.
func TestClonesDoNotOverlapMirrorWrites(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()
	ctx := context.Background()
	// The upstream stands still between its commits. git 2.54 otherwise
	// repacks it, detached, after each one, under the next one and under the
	// fetch reading it: a race of git's own this test is not about.
	runGit(t, f.up.dir, "config", "maintenance.auto", "false")

	const readers = 4
	const rounds = 40
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	clones := 0

	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; ; k++ {
				select {
				case <-stop:
					return
				default:
				}
				ws, err := mirror.Acquire(ctx, fmt.Sprintf("run_w%d_%d", r, k), "refs/heads/main")
				mu.Lock()
				if err != nil {
					failures = append(failures, err)
				} else {
					clones++
				}
				mu.Unlock()
				if err == nil {
					_ = ws.Remove()
				}
			}
		}()
	}

	for i := range rounds {
		// Enough objects per round that a fetch, and the repack after it,
		// rewrite real files rather than one small one.
		for j := range 8 {
			f.up.commit(fmt.Sprintf("dir%d/file%d.txt", j, i),
				strings.Repeat(fmt.Sprintf("round %d file %d\n", i, j), 200), fmt.Sprintf("round %d.%d", i, j))
		}
		if _, err := f.mgr.Fetch(ctx, f.repo); err != nil {
			t.Errorf("round %d: Fetch: %v", i, err)
			break
		}
		unlock, err := mirrorlock.Lock(ctx, mirror.Path(), true)
		if err != nil {
			t.Errorf("round %d: locking the mirror for gc: %v", i, err)
			break
		}
		runGit(t, mirror.Path(), "repack", "-a", "-d", "-q")
		unlock()
	}
	close(stop)
	wg.Wait()

	for _, err := range failures {
		t.Errorf("a run failed while the mirror was being written: %v", err)
	}
	if clones == 0 {
		t.Fatal("no clone completed: the test raced nothing")
	}
}
