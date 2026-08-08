//go:build integration

// These tests need a Docker daemon, which is why they are behind a build tag:
// `go test ./...` on a laptop with no daemon must still be a meaningful gate.
// Run them with `go test -tags=integration ./internal/cache/...`.
//
// What they cover that the fake cannot: that VolumeCreate really is idempotent
// for an existing name, that /system/df is really where a volume's size comes
// from, and that the label filter really finds the volumes we created. All
// three are assumptions about the daemon, and a fake would only restate them.
package cache

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

func dockerOrSkip(t *testing.T) DockerAPI {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return NewDockerAPI(cli)
}

func TestIntegrationVolumeLifecycle(t *testing.T) {
	api := dockerOrSkip(t)
	ctx := context.Background()

	const runner, cache = "itest", "pnpm"
	name := VolumeName(runner, cache)
	labels := map[string]string{LabelManaged: "true", LabelRunner: runner, LabelCache: cache}

	t.Cleanup(func() { _ = api.VolumeRemove(context.Background(), name) })

	if err := api.VolumeEnsure(ctx, name, labels); err != nil {
		t.Fatal(err)
	}
	// Every run of every job calls this; it has to be idempotent.
	if err := api.VolumeEnsure(ctx, name, labels); err != nil {
		t.Fatalf("VolumeEnsure is not idempotent: %v", err)
	}

	vols, err := api.VolumeList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *VolumeInfo
	for i := range vols {
		if vols[i].Name == name {
			found = &vols[i]
		}
	}
	if found == nil {
		t.Fatalf("volume %s is not in the managed listing", name)
	}
	// The name is not parseable back into its parts; the labels are the truth.
	if found.Runner != runner || found.Cache != cache {
		t.Errorf("labels round-tripped as %+v", found)
	}

	if err := api.VolumeRemove(ctx, name); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationAcquireThroughTheManager(t *testing.T) {
	api := dockerOrSkip(t)
	ctx := context.Background()

	l, err := New(Options{
		Root:        t.TempDir(),
		Docker:      api,
		LockTimeout: 5 * time.Second,
		Runners:     map[string]RunnerConfig{"itest": {Executor: ExecutorDocker}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.VolumeRemove(context.Background(), VolumeName("itest", "gradle")) })

	mounts, err := l.Acquire(ctx, "itest", []Request{{Name: "gradle", Target: "/cache/gradle"}})
	if err != nil {
		t.Fatal(err)
	}
	if mounts[0].Source != VolumeName("itest", "gradle") {
		t.Errorf("Source = %q", mounts[0].Source)
	}

	infos, err := l.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, i := range infos {
		if i.Source == mounts[0].Source {
			seen = true
			if i.LastUsedAt.IsZero() {
				t.Error("LastUsedAt was not stamped at Acquire")
			}
		}
	}
	if !seen {
		t.Errorf("the acquired volume is missing from List: %+v", infos)
	}

	if err := l.Release(ctx, "itest", []string{"gradle"}); err != nil {
		t.Fatal(err)
	}
}

// The build cache prune is the one call whose filter syntax the daemon parses
// rather than the client, so a wrong duration format only shows up here.
func TestIntegrationBuildCachePrune(t *testing.T) {
	api := dockerOrSkip(t)

	if _, err := api.BuildCachePrune(context.Background(), buildCacheKeep); err != nil {
		t.Fatalf("the daemon rejected the until filter: %v", err)
	}
}
