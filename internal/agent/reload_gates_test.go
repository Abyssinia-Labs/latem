package agent

import (
	"context"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A54, end to end: a repo added to config.toml and reloaded is one the run
// gates admit, with no restart — which is what `latem repo add` promises. The
// same run before the reload is E_UNKNOWN_REPO, so the test proves the gates
// moved rather than that they were open.
func TestReload_TheRunGatesAdmitAnAddedRepo(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed; the registry is read from a real checkout (INV-5)")
	}
	cfg := newConfigDir(t, configOptions{})
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	enqueue := func(id string) {
		t.Helper()
		db := openStore(t, cfg)
		ctx := context.Background()
		if err := db.Create(ctx, store.Run{ID: id, Repo: "added", Job: "test", Ref: "main",
			State: protocol.StateQueued, OriginKind: "cli", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := db.Enqueue(ctx, id, 0); err != nil {
			t.Fatal(err)
		}
	}

	enqueue("r_before")
	if r := waitState(t, cfg, "r_before", 30*time.Second, protocol.StateRejected); r.ErrorCode != protocol.ErrUnknownRepo {
		t.Fatalf("before the reload: %s %s, want rejected %s", r.State, r.ErrorCode, protocol.ErrUnknownRepo)
	}

	appendConfig(t, cfg, "\n[repos.added]\nurl          = \""+repo+"\"\nallowed_refs = [\"main\"]\n")
	deferred, err := ta.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if len(deferred) != 0 {
		t.Errorf("an orchestrator with SetConfig still deferred %v", deferred)
	}

	enqueue("r_after")
	if r := waitState(t, cfg, "r_after", 60*time.Second, protocol.StateSucceeded, protocol.StateFailed,
		protocol.StateRejected); r.State != protocol.StateSucceeded {
		t.Errorf("after the reload: %s %s %s, want succeeded", r.State, r.ErrorCode, r.ErrorMsg)
	}
}
