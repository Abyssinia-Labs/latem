package agent

import (
	"context"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
)

// maintenanceLoop is the daily ticker of §14.4, §16, and §11.
//
// It does not run at startup. Reconciliation already swept the expired runs
// and reaped the orphans, and a fresh agent spending its first minute walking
// a 25 GB cache tree is an agent that looks broken to whoever just started it.
// The first sweep is one interval away.
func (a *Agent) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(a.maintenanceEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Maintain(ctx)
		}
	}
}

// Maintain reclaims space and expires waiting runs.
//
// Every sweep runs even if an earlier one failed, and every one logs what it
// reclaimed. That second half is not decoration: RUNNER_SPEC §14.4 says a
// prune that silently drops a warm cache is indistinguishable, from the next
// build's point of view, from a bug — the build is simply slow, for a month,
// and nobody attributes it to a ticker. The same is true of a log somebody was
// about to read.
func (a *Agent) Maintain(ctx context.Context) {
	a.maintainQueue(ctx)
	a.maintainLogs()
	a.maintainArtifacts()
	a.maintainCaches(ctx)
}

// maintainQueue is §11's TTL sweep, run daily as well as at startup.
func (a *Agent) maintainQueue(ctx context.Context) {
	ids, err := a.sweepExpired(ctx)
	if err != nil {
		a.log.Error("sweeping expired runs", "err", err)
	}
	if len(ids) > 0 {
		a.log.Info("swept expired runs", "count", len(ids), "runs", ids)
	}
}

// maintainLogs applies §16's retention: gzip after a day, delete after
// log_retention.
func (a *Agent) maintainLogs() {
	report, err := logstream.Prune(a.cfg.Paths.Logs, logstream.PruneOptions{
		Retention: a.logRetention(),
	}, a.now(), a.isLive)
	if err != nil {
		a.log.Error("pruning logs", "dir", a.cfg.Paths.Logs, "err", err)
	}
	if len(report.Compressed) > 0 || len(report.Deleted) > 0 {
		a.log.Info("log retention",
			"compressed", len(report.Compressed),
			"deleted", len(report.Deleted),
			"freed_bytes", report.BytesFreed,
			"retention", a.logRetention())
	}
}

// maintainArtifacts applies §17's per-declaration retention.
func (a *Agent) maintainArtifacts() {
	report, err := artifact.Prune(a.cfg.Paths.Artifacts, artifact.PruneOptions{
		Now:  a.now(),
		Live: a.isLive,
	})
	if err != nil {
		a.log.Error("pruning artifacts", "dir", a.cfg.Paths.Artifacts, "err", err)
	}
	if len(report.FilesExpired) > 0 || len(report.RunsEvicted) > 0 {
		a.log.Info("artifact retention",
			"files_expired", len(report.FilesExpired),
			"runs_evicted", report.RunsEvicted,
			"freed_bytes", report.BytesFreed,
			"total_bytes", report.TotalBytes)
	}
}

// maintainCaches is §14.4: build layers, oversized volumes, stale workspaces,
// mirror gc, and leaked Tart VMs.
//
// Aggressive mode is entered when any runner is below its floor, because at
// that point the agent is already refusing runs with E_DISK and a polite prune
// is the wrong trade.
func (a *Agent) maintainCaches(ctx context.Context) {
	if a.caches == nil {
		return
	}
	report, err := a.caches.Prune(ctx, cache.PruneOptions{
		Aggressive: a.belowAnyFloor(ctx),
	})
	if err != nil {
		a.log.Error("pruning caches", "err", err)
	}
	if report.BytesReclaimed > 0 || len(report.VolumesEvicted) > 0 ||
		len(report.WorkspacesGone) > 0 || len(report.VMsReaped) > 0 ||
		len(report.MirrorsCollected) > 0 {
		a.log.Info("cache prune",
			"freed_bytes", report.BytesReclaimed,
			"volumes_evicted", report.VolumesEvicted,
			"workspaces_removed", report.WorkspacesGone,
			"vms_reaped", report.VMsReaped,
			"mirrors_collected", report.MirrorsCollected)
	}

	if stale, serr := a.caches.StaleMirrors(90 * 24 * time.Hour); serr == nil && len(stale) > 0 {
		// Reported, never deleted: a mirror may be the only local copy of a
		// repository (§14.4).
		a.log.Info("mirrors untouched for 90 days (not deleted)", "mirrors", stale)
	}
	a.recordCaches(ctx)
}

func (a *Agent) belowAnyFloor(ctx context.Context) bool {
	if a.caches == nil {
		return false
	}
	free, err := a.caches.FreeGB(ctx)
	if err != nil {
		return false
	}
	for _, r := range a.runners {
		if free < a.caches.MinFreeGB(r.name) {
			return true
		}
	}
	return false
}

func (a *Agent) logRetention() time.Duration {
	if d := a.cfg.Agent.LogRetention.Duration(); d > 0 {
		return d
	}
	return config.DefaultLogRetention
}
