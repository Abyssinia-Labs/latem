package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestFetch_CreatesABareMirrorWithAutoGCOff(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	if want := filepath.Join(f.mgr.MirrorDir(), "myapp.git"); mirror.Path() != want {
		t.Errorf("mirror path = %s, want %s", mirror.Path(), want)
	}
	if got := runGit(t, mirror.Path(), "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("mirror is not bare: %s", got)
	}
	if got := runGit(t, mirror.Path(), "config", "--get", "gc.auto"); got != "0" {
		t.Fatalf("gc.auto = %q, want 0", got)
	}
	if got := runGit(t, mirror.Path(), "config", "--get", "remote.origin.mirror"); got != "true" {
		t.Errorf("remote.origin.mirror = %q, want true", got)
	}
}

func TestFetch_IsIncremental(t *testing.T) {
	f := newFixture(t)
	f.fetch()

	sha := f.up.commit("later.txt", "later\n", "a commit made after the mirror existed")
	mirror := f.fetch()

	got, err := mirror.Resolve(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != sha {
		t.Errorf("Resolve = %s, want the new tip %s", got, sha)
	}
}

// TestFetch_RepairsAutoGCOnAMirrorThatPredatesTheRule. A mirror created by an
// older agent has gc.auto at its default, and `git fetch` is one of the
// commands that triggers automatic gc — so the repair has to happen before the
// fetch, not after it.
func TestFetch_RepairsAutoGCOnAMirrorThatPredatesTheRule(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()
	runGit(t, mirror.Path(), "config", "gc.auto", "6700")

	f.fetch()

	if got := runGit(t, mirror.Path(), "config", "--get", "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q after a second fetch, want 0", got)
	}
}

// TestFetch_FollowsAURLChangeInConfig. A repo whose URL moved otherwise keeps
// fetching from the old host forever, and the symptom is a ref that exists
// upstream and cannot be found here.
func TestFetch_FollowsAURLChangeInConfig(t *testing.T) {
	f := newFixture(t)
	f.fetch()

	moved := newUpstream(t, filepath.Join(f.root, "moved"))
	sha := moved.commit("moved.txt", "moved\n", "a commit only the new host has")
	f.repo.URL = moved.dir

	mirror := f.fetch()
	got, err := mirror.Resolve(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("Resolve after the URL moved: %v", err)
	}
	if got != sha {
		t.Errorf("Resolve = %s, want %s from the new host", got, sha)
	}
	if url := runGit(t, mirror.Path(), "remote", "get-url", "origin"); url != moved.dir {
		t.Errorf("origin = %q, want %q", url, moved.dir)
	}
}

// TestFetch_NeverLeavesAPartialMirrorAtTheFinalPath. A crash partway through
// cloning a 2 GB repo would otherwise leave a truncated object store where the
// next run's existence check sees a directory, skips creation, and fetches
// into a repository missing most of its objects.
func TestFetch_NeverLeavesAPartialMirrorAtTheFinalPath(t *testing.T) {
	f := newFixture(t)
	f.repo.URL = filepath.Join(f.root, "there-is-no-repo-here")

	if _, err := f.mgr.Fetch(context.Background(), f.repo); err == nil {
		t.Fatal("mirrored a repository that does not exist")
	} else if got := CodeOf(err); got != protocol.ErrSource {
		t.Errorf("code = %s, want %s", got, protocol.ErrSource)
	}

	if _, err := os.Stat(filepath.Join(f.mgr.MirrorDir(), "myapp.git")); !os.IsNotExist(err) {
		t.Errorf("a failed clone left something at the final mirror path (err=%v)", err)
	}
	entries, err := os.ReadDir(f.mgr.MirrorDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("mirror dir holds %v after a failed clone, want nothing", names)
	}
}

// TestFetch_ReportsAnUnreachableUpstream. The mirror already exists, so this
// is the ordinary "the git host is down" case, and it must be E_SOURCE —
// retryable — rather than something that rejects the run permanently.
func TestFetch_ReportsAnUnreachableUpstream(t *testing.T) {
	f := newFixture(t)
	f.fetch()

	if err := os.RemoveAll(f.up.dir); err != nil {
		t.Fatal(err)
	}
	_, err := f.mgr.Fetch(context.Background(), f.repo)
	if err == nil {
		t.Fatal("fetched from an upstream that no longer exists")
	}
	if got := CodeOf(err); got != protocol.ErrSource {
		t.Errorf("code = %s, want %s", got, protocol.ErrSource)
	}
	if !protocol.ErrSource.Retryable() {
		t.Error("E_SOURCE is not retryable; a transient host outage would be permanent")
	}
}

// TestFetch_ReportsAnUnusableMirrorPath covers the stat branch that is neither
// "there" nor "not there": something is at the mirror path that cannot be
// inspected or used.
func TestFetch_ReportsAnUnusableMirrorPath(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.mgr.MirrorDir(), "myapp.git"), []byte("not a repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Fetch(context.Background(), f.repo); err == nil {
		t.Fatal("fetched into a regular file")
	}
}

func TestFetch_RejectsRepoKeysAndURLs(t *testing.T) {
	f := newFixture(t)

	cases := map[string]Repo{
		"empty key":          {Key: "", URL: f.up.dir},
		"traversal key":      {Key: "../evil", URL: f.up.dir},
		"dot dot key":        {Key: "..", URL: f.up.dir},
		"dot dot mid-key":    {Key: "a..b", URL: f.up.dir},
		"slash key":          {Key: "a/b", URL: f.up.dir},
		"leading dot key":    {Key: ".hidden", URL: f.up.dir},
		"leading dash key":   {Key: "-x", URL: f.up.dir},
		"null in key":        {Key: "a\x00b", URL: f.up.dir},
		"newline in key":     {Key: "a\nb", URL: f.up.dir},
		"overlong key":       {Key: strings.Repeat("a", 65), URL: f.up.dir},
		"empty url":          {Key: "ok", URL: ""},
		"option url":         {Key: "ok", URL: "--upload-pack=/bin/sh"},
		"ext transport":      {Key: "ok", URL: "ext::sh -c 'touch /tmp/pwned'"},
		"ext uppercase":      {Key: "ok", URL: "EXT::sh -c id"},
		"newline in url":     {Key: "ok", URL: "https://example.com/x\n[core]\n\tpager=sh"},
		"overlong url":       {Key: "ok", URL: "https://example.com/" + strings.Repeat("a", 2048)},
		"key and url hostil": {Key: "$(id)", URL: "`id`"},
	}

	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.mgr.Fetch(context.Background(), repo); err == nil {
				t.Fatalf("accepted %+v", repo)
			} else if got := CodeOf(err); got != protocol.ErrUnknownRepo {
				t.Errorf("code = %s, want %s", got, protocol.ErrUnknownRepo)
			}
		})
	}
	assertNoSentinel(t, f.root)
}

func TestHasRef(t *testing.T) {
	f := newFixture(t)
	f.up.branch("release/1.2")
	f.up.annotatedTag("v1.0", "release one")
	mirror := f.fetch()

	cases := map[string]struct {
		ref  string
		want bool
	}{
		"branch":            {"refs/heads/main", true},
		"nested branch":     {"refs/heads/release/1.2", true},
		"annotated tag":     {"refs/tags/v1.0", true},
		"missing branch":    {"refs/heads/nope", false},
		"missing tag":       {"refs/tags/nope", false},
		"branch spelt tag":  {"refs/tags/main", false},
		"synthesised pr ns": {"refs/pull/1/head", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := mirror.HasRef(context.Background(), tc.ref)
			if err != nil {
				t.Fatalf("HasRef(%s): %v", tc.ref, err)
			}
			if got != tc.want {
				t.Errorf("HasRef(%s) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

// TestHasRef_RequiresAFullyQualifiedName. show-ref --verify reports false for
// a short name that does exist, so accepting one would present a
// normalization bug as a missing branch.
func TestHasRef_RequiresAFullyQualifiedName(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	if _, err := mirror.HasRef(context.Background(), "main"); err == nil {
		t.Fatal("HasRef accepted a short name")
	} else if got := CodeOf(err); got != protocol.ErrInvalidRef {
		t.Errorf("code = %s, want %s", got, protocol.ErrInvalidRef)
	}
}

// TestNormalizeRef_ThroughARealMirror wires gate 7 to its real ref lister. The
// unit tests in internal/registry use a fake; this is the one that would catch
// HasRef answering a question subtly different from the one NormalizeRef asks.
func TestNormalizeRef_ThroughARealMirror(t *testing.T) {
	f := newFixture(t)
	f.up.branch("release/1.2")
	f.up.annotatedTag("v1.0", "release one")
	// "shared" exists as both a branch and a tag, which must never be guessed.
	f.up.branch("shared")
	runGit(t, f.up.dir, "tag", "shared")
	mirror := f.fetch()

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"main", "refs/heads/main"},
		{"refs/heads/main", "refs/heads/main"},
		{"release/1.2", "refs/heads/release/1.2"},
		{"v1.0", "refs/tags/v1.0"},
		{"refs/tags/v1.0", "refs/tags/v1.0"},
	} {
		got, err := registry.NormalizeRef(context.Background(), mirror, tc.in)
		if err != nil {
			t.Errorf("NormalizeRef(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := registry.NormalizeRef(context.Background(), mirror, "shared"); registry.CodeOf(err) != protocol.ErrAmbiguousRef {
		t.Errorf("a name that is both a branch and a tag gave %v, want E_AMBIGUOUS_REF", err)
	}
	if _, err := registry.NormalizeRef(context.Background(), mirror, "nope"); registry.CodeOf(err) != protocol.ErrInvalidRef {
		t.Errorf("an absent ref gave %v, want E_INVALID_REF", err)
	}
}

// TestResolve_AnnotatedTagPeelsToItsCommit is why §12 writes ${REF}^{commit}.
// refs/tags/v1.0 on an annotated tag names a tag *object*; recording that as
// the run's SHA records a value git log will not accept, and checking it out
// produces something surprising.
func TestResolve_AnnotatedTagPeelsToItsCommit(t *testing.T) {
	f := newFixture(t)
	commitSHA := f.up.commit("tagged.txt", "tagged\n", "the commit the tag points at")
	f.up.annotatedTag("v1.0", "release one")
	mirror := f.fetch()

	tagObject := runGit(t, mirror.Path(), "rev-parse", "refs/tags/v1.0")
	if tagObject == commitSHA {
		t.Fatal("fixture produced a lightweight tag; the peel would be untested")
	}

	got, err := mirror.Resolve(context.Background(), "refs/tags/v1.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != commitSHA {
		t.Errorf("Resolve = %s, want the commit %s (got the tag object %s?)", got, commitSHA, tagObject)
	}

	// And the workspace really is at the commit, not at the tag object.
	ws := f.acquire("run_tag", "refs/tags/v1.0")
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != commitSHA {
		t.Errorf("workspace HEAD = %s, want %s", head, commitSHA)
	}
	if kind := runGit(t, ws.Path, "cat-file", "-t", "HEAD"); kind != "commit" {
		t.Errorf("HEAD is a %s, want a commit", kind)
	}
}

func TestResolve_LightweightTagAndBranch(t *testing.T) {
	f := newFixture(t)
	sha := f.up.commit("x.txt", "x\n", "second")
	runGit(t, f.up.dir, "tag", "light")
	mirror := f.fetch()

	for _, ref := range []string{"refs/heads/main", "refs/tags/light"} {
		got, err := mirror.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", ref, err)
		}
		if got != sha {
			t.Errorf("Resolve(%s) = %s, want %s", ref, got, sha)
		}
	}
}

// TestResolve_TreeAndBlobRefsFailLoudly. §12 says ${REF}^{commit} "fails
// loudly on non-commit refs rather than checking out something surprising".
func TestResolve_NonCommitRefsFailLoudly(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	blob := runGit(t, mirror.Path(), "rev-parse", "refs/heads/main:README.md")
	runGit(t, mirror.Path(), "update-ref", "refs/latem-test/blob", blob)

	if _, err := mirror.Resolve(context.Background(), "refs/latem-test/blob"); err == nil {
		t.Fatal("resolved a ref pointing at a blob")
	} else if got := CodeOf(err); got != protocol.ErrSource {
		t.Errorf("code = %s, want %s", got, protocol.ErrSource)
	}
}

func TestResolve_MissingRefIsAnError(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	if _, err := mirror.Resolve(context.Background(), "refs/heads/nope"); err == nil {
		t.Fatal("resolved a ref that does not exist")
	}
}

// TestForcePush_DoesNotOrphanACommitAnInFlightRunIsUsing is the reason
// gc.auto 0 is described as "not optional" in §12.
//
// The sequence is the real one: a run resolves a SHA at gate 9, someone force-
// pushes, the next run's `fetch --prune` makes the old tip unreachable, and
// automatic gc is then free to collect a commit a build is forty minutes into.
// The test forces the issue by running gc between gate 9 and gate 10.
func TestForcePush_DoesNotOrphanACommitAnInFlightRunIsUsing(t *testing.T) {
	f := newFixture(t)
	original := f.up.commit("v1.txt", "one\n", "the commit an in-flight run resolved")

	mirror := f.fetch()
	inFlight, err := mirror.Resolve(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if inFlight != original {
		t.Fatalf("Resolve = %s, want %s", inFlight, original)
	}

	// Upstream history is rewritten and the old tip becomes unreachable here.
	rewritten := f.up.forcePush("rewritten\n")
	if rewritten == original {
		t.Fatal("fixture did not actually rewrite history")
	}
	f.fetch()

	// The moment automatic gc would have fired.
	runGit(t, mirror.Path(), "gc", "--auto")
	runGit(t, mirror.Path(), "gc", "--auto", "--quiet")

	present, err := mirror.HasCommit(context.Background(), inFlight)
	if err != nil {
		t.Fatalf("HasCommit: %v", err)
	}
	if !present {
		t.Fatal("the in-flight commit was collected; gc.auto 0 is not in effect")
	}

	// And gate 10 still succeeds, which is what the run actually needs.
	ws, err := mirror.Checkout(context.Background(), "run_inflight", "refs/heads/main", inFlight)
	if err != nil {
		t.Fatalf("Checkout of the in-flight commit after a force-push: %v", err)
	}
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != inFlight {
		t.Errorf("HEAD = %s, want %s", head, inFlight)
	}

	// Meanwhile the mirror has genuinely followed the force-push, so this is
	// not passing because the fetch did nothing.
	nowAt, err := mirror.Resolve(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if nowAt != rewritten {
		t.Errorf("mirror main = %s, want the rewritten tip %s", nowAt, rewritten)
	}
}

func TestHasCommit_RejectsUnresolvedNames(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	if _, err := mirror.HasCommit(context.Background(), "main"); err == nil {
		t.Error("HasCommit accepted a ref name")
	}
	absent := strings.Repeat("0", 39) + "1"
	got, err := mirror.HasCommit(context.Background(), absent)
	if err != nil {
		t.Fatalf("HasCommit on an absent object: %v", err)
	}
	if got {
		t.Error("HasCommit reported an object that does not exist")
	}
}

func TestMirror_Accessors(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()
	if mirror.Repo().Key != "myapp" {
		t.Errorf("Repo().Key = %q", mirror.Repo().Key)
	}
	if !strings.HasSuffix(mirror.Path(), "myapp.git") {
		t.Errorf("Path() = %q", mirror.Path())
	}
}

func TestFetch_CancelledContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.mgr.Fetch(ctx, f.repo); err == nil {
		t.Fatal("fetched with a cancelled context")
	}
}
