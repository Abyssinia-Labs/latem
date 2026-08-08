package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// APIClient is the subset of the Docker Engine API this package uses.
//
// Two reasons for narrowing it rather than passing *client.Client around.
// First, everything that does not genuinely need a daemon — argv construction,
// label building, $LATEM_ENV merging, cache-mount validation — becomes
// testable in the default `go test` gate, which is where the F1 regressions
// would show up. Second, the signatures here are the ones this package wants
// rather than the ones the SDK happens to expose: ContainerCreate loses the
// networking and platform arguments nothing sets, so a reader does not have to
// wonder what the two nils mean.
//
// [sdkClient] adapts the real SDK client to it.
type APIClient interface {
	Ping(ctx context.Context) (types.Ping, error)

	ImageInspect(ctx context.Context, ref string) (image.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)

	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, options container.StartOptions) error
	ContainerKill(ctx context.Context, id, signal string) error
	ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerWait(ctx context.Context, id string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error)

	ContainerExecCreate(ctx context.Context, id string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)

	CopyFromContainer(ctx context.Context, id, srcPath string) (io.ReadCloser, container.PathStat, error)
	CopyToContainer(ctx context.Context, id, dstPath string, content io.Reader, options container.CopyToContainerOptions) error

	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)

	Close() error
}

// sdkClient adapts *client.Client to [APIClient]. Every method not listed here
// is promoted from the embedded client unchanged.
type sdkClient struct{ *client.Client }

var _ APIClient = sdkClient{}

func (c sdkClient) ImageInspect(ctx context.Context, ref string) (image.InspectResponse, error) {
	return c.Client.ImageInspect(ctx, ref)
}

func (c sdkClient) ContainerCreate(
	ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	name string,
) (container.CreateResponse, error) {
	// nil networking config and nil platform: a run's container joins the
	// daemon's default bridge and runs the image's own platform. Anything else
	// would be a config surface RUNNER_SPEC §8 does not describe.
	return c.Client.ContainerCreate(ctx, config, hostConfig, nil, nil, name)
}

// newSDKClient builds a client from the ambient DOCKER_* environment, with API
// version negotiation so one agent binary works against the range of daemons
// an operator might actually be running.
func newSDKClient() (APIClient, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "docker client")
	}
	return sdkClient{Client: c}, nil
}

// isNotFound reports whether err is the daemon saying "no such object". It is
// separated out because "already gone" is success for teardown and for reading
// a $LATEM_ENV file no step ever created.
//
// The SDK deprecates client.IsErrNotFound in favour of containerd's
// cerrdefs.IsNotFound, which it is a one-line alias for. cerrdefs is not a
// direct dependency of this module, and PLAN §2 rule 2 gives go.mod exactly one
// owner (WS-00), so this package uses the API its direct dependency exposes
// rather than adding one. Reported to WS-00 rather than worked around here.
//
//nolint:staticcheck // SA1019: see above — the replacement lives in a module this workstream may not add to go.mod.
func isNotFound(err error) bool {
	return err != nil && client.IsErrNotFound(err)
}
