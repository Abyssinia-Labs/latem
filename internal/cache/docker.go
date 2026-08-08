package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// DockerAPI is the slice of the Docker daemon this package needs.
//
// It exists as an interface for one reason: everything else in the cache
// subsystem is testable against a real filesystem in a t.TempDir, and the
// volume half should not drag a daemon into `go test ./...`. The real
// implementation is [NewDockerAPI]; tests that must exercise it live behind
// the `integration` build tag.
//
// WS-07 owns the Docker executor and its own client. This interface
// deliberately does not reach into that package: the cache subsystem needs
// four calls, not a container runtime.
type DockerAPI interface {
	// VolumeEnsure creates the named volume if it does not exist. It must be
	// idempotent — every run of every job calls it.
	VolumeEnsure(ctx context.Context, name string, labels map[string]string) error
	// VolumeList reports the managed cache volumes and their sizes.
	VolumeList(ctx context.Context) ([]VolumeInfo, error)
	// VolumeRemove deletes a volume.
	VolumeRemove(ctx context.Context, name string) error
	// BuildCachePrune drops build layers untouched for keep, returning the
	// bytes reclaimed (RUNNER_SPEC §14.4).
	BuildCachePrune(ctx context.Context, keep time.Duration) (int64, error)
}

// VolumeInfo is one managed cache volume as the daemon reports it.
type VolumeInfo struct {
	Name   string
	Runner string
	Cache  string
	// SizeBytes is -1 when the daemon cannot report a size, which is the case
	// for every non-local volume driver. Callers must not treat that as zero:
	// "unknown" and "empty" lead to opposite eviction decisions.
	SizeBytes int64
}

// dockerAPI adapts the Docker client to [DockerAPI].
type dockerAPI struct{ cli client.APIClient }

// NewDockerAPI wraps a Docker client for use as [Options.Docker].
func NewDockerAPI(cli client.APIClient) DockerAPI { return dockerAPI{cli: cli} }

func (d dockerAPI) VolumeEnsure(ctx context.Context, name string, labels map[string]string) error {
	// VolumeCreate is idempotent for an existing volume with the same name and
	// driver: the daemon returns the existing volume rather than an error, so
	// there is no inspect-then-create race to lose.
	if _, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		Labels: labels,
	}); err != nil {
		return fmt.Errorf("cache: creating volume %s: %w", name, err)
	}
	return nil
}

func (d dockerAPI) VolumeList(ctx context.Context) ([]VolumeInfo, error) {
	// Sizes come from /system/df rather than /volumes, because the volume list
	// endpoint does not populate UsageData. Without a size, §14.4's cap is
	// unenforceable, so the more expensive call is the necessary one.
	usage, err := d.cli.DiskUsage(ctx, types.DiskUsageOptions{
		Types: []types.DiskUsageObject{types.VolumeObject},
	})
	if err != nil {
		return nil, fmt.Errorf("cache: reading docker disk usage: %w", err)
	}

	var out []VolumeInfo
	for _, v := range usage.Volumes {
		if v == nil || v.Labels[LabelManaged] != "true" {
			continue
		}
		info := VolumeInfo{
			Name:      v.Name,
			Runner:    v.Labels[LabelRunner],
			Cache:     v.Labels[LabelCache],
			SizeBytes: -1,
		}
		if v.UsageData != nil {
			info.SizeBytes = v.UsageData.Size
		}
		out = append(out, info)
	}
	return out, nil
}

func (d dockerAPI) VolumeRemove(ctx context.Context, name string) error {
	// force=false on purpose. A volume still referenced by a container is one
	// a run is using, and the daemon refusing is the correct outcome — the
	// pruner's own lock check should already have skipped it, and this is the
	// backstop for the case where it did not.
	if err := d.cli.VolumeRemove(ctx, name, false); err != nil {
		return fmt.Errorf("cache: removing volume %s: %w", name, err)
	}
	return nil
}

func (d dockerAPI) BuildCachePrune(ctx context.Context, keep time.Duration) (int64, error) {
	args := filters.NewArgs()
	args.Add("until", keep.String())

	report, err := d.cli.BuildCachePrune(ctx, build.CachePruneOptions{Filters: args})
	if err != nil {
		return 0, fmt.Errorf("cache: pruning build cache: %w", err)
	}
	if report == nil {
		return 0, nil
	}
	// #nosec G115 -- SpaceReclaimed is a byte count the daemon just measured on
	// a local filesystem. Exceeding MaxInt64 would mean it reclaimed eight
	// exabytes, and the Report field it feeds is an int64 on the frozen
	// interface, so there is nowhere wider to put it.
	return int64(report.SpaceReclaimed), nil
}
