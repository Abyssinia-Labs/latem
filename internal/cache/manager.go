// Package cache manages mounted build caches.
//
// This file is owned by WS-00 and frozen. The implementation is WS-08.
//
// The design point, from RUNNER_SPEC §14: hosted CI needs save and restore
// because its machines are ephemeral. Ours are not. Caches are mounted, not
// archived — no tarball, no upload, no size ceiling, and no cache step in the
// job.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

// ErrLockTimeout means a per-cache lock could not be acquired within the
// timeout. It maps to protocol.ErrCache. Failing is deliberate: hanging
// forever on a contended cache is worse than a run that says why it stopped.
var ErrLockTimeout = errors.New("cache: lock acquisition timed out")

// Manager provisions caches for runs and reclaims space between them.
type Manager interface {
	// Acquire prepares the named caches for a run and returns their mounts.
	//
	// Caches are scoped per runner, never shared between runners, because two
	// runs writing one ~/.gradle corrupt each other in ways that surface as
	// intermittent build failures nobody attributes to caching. Within a
	// runner at concurrency > 1, this takes a per-cache flock held until
	// [Release], and returns [ErrLockTimeout] rather than blocking forever.
	Acquire(ctx context.Context, runner string, names []string) ([]executor.CacheMount, error)

	// Release drops the locks taken by [Acquire]. It must be idempotent and
	// safe to call after a failed Acquire.
	Release(ctx context.Context, runner string, names []string) error

	// List reports known caches and their sizes.
	List(ctx context.Context) ([]Info, error)

	// Prune reclaims space per RUNNER_SPEC §14.4: build layers, oversized
	// volumes by access time, stale workspaces, mirror gc, and leaked Tart VMs.
	Prune(ctx context.Context, opts PruneOptions) (Report, error)

	// FreeGB reports free space on the filesystem holding the work directory,
	// for the disk gates at RUNNER_SPEC §10 steps 5 and 17.
	FreeGB(ctx context.Context) (float64, error)
}

// Info describes one cache.
type Info struct {
	Name       string
	Runner     string
	Source     string // volume name or host directory
	SizeBytes  int64
	LastUsedAt time.Time
}

// PruneOptions bounds a prune pass.
type PruneOptions struct {
	// Aggressive is set when free disk has fallen below a runner's floor. It
	// permits evicting caches that would otherwise be kept.
	Aggressive bool
	// MaxVolumeBytes is the per-volume ceiling; oversized volumes are evicted
	// by access time down to 80% of it.
	MaxVolumeBytes int64
	// WorkspaceMaxAge deletes workspaces older than this with no live run.
	WorkspaceMaxAge time.Duration
}

// Report summarises what a prune reclaimed. Callers should log it: a prune
// that silently drops a warm cache is indistinguishable, from the next build's
// perspective, from a bug.
type Report struct {
	BytesReclaimed   int64
	VolumesEvicted   []string
	WorkspacesGone   []string
	VMsReaped        []string
	MirrorsCollected []string
}

// NotImplemented satisfies [Manager] by refusing every call.
type NotImplemented struct{}

var _ Manager = NotImplemented{}

func (NotImplemented) Acquire(context.Context, string, []string) ([]executor.CacheMount, error) {
	return nil, fmt.Errorf("cache: Acquire: %w", errors.ErrUnsupported)
}

func (NotImplemented) Release(context.Context, string, []string) error {
	return fmt.Errorf("cache: Release: %w", errors.ErrUnsupported)
}

func (NotImplemented) List(context.Context) ([]Info, error) {
	return nil, fmt.Errorf("cache: List: %w", errors.ErrUnsupported)
}

func (NotImplemented) Prune(context.Context, PruneOptions) (Report, error) {
	return Report{}, fmt.Errorf("cache: Prune: %w", errors.ErrUnsupported)
}

func (NotImplemented) FreeGB(context.Context) (float64, error) {
	return 0, fmt.Errorf("cache: FreeGB: %w", errors.ErrUnsupported)
}
