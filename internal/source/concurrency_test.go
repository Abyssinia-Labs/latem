package source

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
