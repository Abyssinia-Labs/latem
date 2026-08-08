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
// which fully-qualified name exists, gates 9 and 10 turn it into a workspace.
//
// It also satisfies [registry.RefLister], which is what lets normalization stay
// a pure function in internal/registry with no git repository behind it.
type Mirror interface {
	HasRef(ctx context.Context, fullyQualified string) (bool, error)
	Acquire(ctx context.Context, runID, normalizedRef string) (*source.Workspace, error)
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
}

// Collector is the subset of [artifact.Collector] the collecting phase uses.
// *artifact.Collector satisfies it directly.
type Collector interface {
	Collect(ctx context.Context, runID, workspace string, decls []artifact.Declaration) ([]protocol.ArtifactRef, error)
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
