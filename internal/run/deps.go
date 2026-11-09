package run

import (
	"context"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Store is [store.Store], aliased so the orchestrator's dependency list reads
// in one place and so a caller wiring it up does not need two imports.
type Store = store.Store

// Mirror is the subset of [source.Mirror] the gate chain uses: gate 7 asks
// which fully-qualified name exists, gate 9 turns it into a commit, gate 10
// turns that commit into a workspace.
//
// The two are separate calls rather than source.Mirror's combined Acquire
// because gate 8 now sits between them (amendment A27). Resolving first is what
// lets a parked run record the commit an operator is being asked to approve;
// checking out the SHA gate 8 was given, rather than re-resolving the ref, is
// what stops the checkout being a second, unchecked resolution of a name that
// may have moved in the meantime.
//
// It also satisfies [registry.RefLister], which is what lets normalization stay
// a pure function in internal/registry with no git repository behind it.
type Mirror interface {
	HasRef(ctx context.Context, fullyQualified string) (bool, error)
	// Resolve is gate 9: the immutable commit a normalized ref names.
	Resolve(ctx context.Context, normalizedRef string) (string, error)
	// Checkout is gate 10: a workspace at exactly that commit.
	Checkout(ctx context.Context, runID, normalizedRef, sha string) (*source.Workspace, error)
	// HasCommit and IsAncestor pin a workflow run's job to the commit its
	// event named, on its ref (A52).
	HasCommit(ctx context.Context, sha string) (bool, error)
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	// FetchCommit fetches a pinned commit by its name, when no ref here
	// reaches it any more.
	FetchCommit(ctx context.Context, sha string) error
	// ContainingRefs and PackSince are a checkout step's: which policy refs
	// hold the commit it moves to, and the objects the workspace lacks for
	// it (A53).
	ContainingRefs(ctx context.Context, sha string, prefixes []string) ([]string, error)
	PackSince(ctx context.Context, sha, have string) ([]byte, error)
}

// Source is the subset of [source.Manager] the gate chain uses.
//
// It is declared here rather than imported because *source.Manager returns
// concrete types and a test needs to hand the orchestrator a mirror with no
// git in it. Consumer-side interfaces are the ordinary Go answer to that, and
// they keep internal/source free of an abstraction only its caller wants.
type Source interface {
	// Fetch is gate 6: make the mirror exist and make it fresh, under the
	// per-repo mutex that serialises concurrent fetches into one bare repo.
	Fetch(ctx context.Context, repo source.Repo) (Mirror, error)
	// Open is the mirror as it is, unfetched, for a commit `latem try` has
	// already put in it (A76). It wraps [source.ErrNoMirror] when there is none.
	Open(repo source.Repo) (Mirror, error)
}

// Collector is the subset of [artifact.Collector] the collecting phase uses.
// *artifact.Collector satisfies it directly.
type Collector interface {
	Collect(ctx context.Context, runID, workspace string, decls []artifact.Declaration) ([]protocol.ArtifactRef, error)
	// The two upload-artifact and download-artifact need (A49). On the same
	// interface rather than a second one, because they are the same store.
	// base is the workspace or the run's temporary directory (A77).
	CollectNamed(ctx context.Context, runID, base string, up artifact.Upload) ([]protocol.ArtifactRef, error)
	Restore(runID, base, name, dest string) (int, error)
	// Names lists a run's stored artifacts, for a download of every one of
	// them across several runs (A84).
	Names(runID string) ([]string, error)
}

// SourceManager adapts a *source.Manager to [Source].
//
// The adapter exists only to narrow *source.Mirror to [Mirror]; it adds no
// behaviour. A nil manager yields nil, so an orchestrator built without source
// acquisition fails at gate 6 with a clear error rather than a nil dereference.
func SourceManager(m *source.Manager) Source {
	if m == nil {
		return nil
	}
	return managerAdapter{m: m}
}

type managerAdapter struct{ m *source.Manager }

func (a managerAdapter) Fetch(ctx context.Context, repo source.Repo) (Mirror, error) {
	mi, err := a.m.Fetch(ctx, repo)
	if err != nil {
		return nil, err
	}
	return mi, nil
}

func (a managerAdapter) Open(repo source.Repo) (Mirror, error) {
	mi, err := a.m.Open(repo)
	if err != nil {
		return nil, err
	}
	return mi, nil
}
