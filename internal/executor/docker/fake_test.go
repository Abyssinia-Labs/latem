package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

// fakeAPI is an in-memory Docker daemon good enough to drive every code path
// in this package that is not the daemon's own behaviour.
//
// It exists so that the things most worth regression-testing — the argv a step
// is executed with, the working directory and environment set on every exec,
// what happens to $LATEM_ENV between steps — are covered by `go test` with no
// daemon anywhere. The integration tests then prove the same properties
// against a real container; this proves them against the code.
type fakeAPI struct {
	mu sync.Mutex

	// Recorded calls.
	createdConfigs []*container.Config
	createdHosts   []*container.HostConfig
	createdNames   []string
	execOpts       []container.ExecOptions
	copiedTo       []copiedFile
	removed        []string
	killed         []string
	volumes        []volume.CreateOptions
	removedVolumes []string

	// Programmed behaviour.
	imageMissing   bool
	pullErr        error
	createErr      error
	startErr       error
	removeErr      error
	execExitCodes  []int // consumed one per exec, defaulting to 0
	execStdout     []string
	execStderr     []string
	blockExec      bool // exec output never arrives until the attach is closed
	envFileData    []byte
	envFileMissing bool
	containers     []container.Summary
	pingErr        error
	waitStatus     int64
	listedVolumes  []*volume.Volume
	repoDigests    []string

	execSeq  int
	closed   bool
	pipeEnds []net.Conn
}

type copiedFile struct {
	dst     string
	entries map[string]string
}

func (f *fakeAPI) Ping(context.Context) (types.Ping, error) {
	return types.Ping{APIVersion: "1.49"}, f.pingErr
}

func (f *fakeAPI) ImageInspect(_ context.Context, ref string) (image.InspectResponse, error) {
	if f.imageMissing {
		return image.InspectResponse{}, errdefs.NotFound(errors.New("no such image: " + ref))
	}
	return image.InspectResponse{RepoDigests: f.repoDigests}, nil
}

func (f *fakeAPI) ImagePull(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	f.mu.Lock()
	f.imageMissing = false
	f.mu.Unlock()
	return io.NopCloser(strings.NewReader(`{"status":"Downloaded"}`)), nil
}

func (f *fakeAPI) ContainerCreate(
	_ context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	name string,
) (container.CreateResponse, error) {
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdConfigs = append(f.createdConfigs, config)
	f.createdHosts = append(f.createdHosts, hostConfig)
	f.createdNames = append(f.createdNames, name)
	return container.CreateResponse{ID: "container-id"}, nil
}

func (f *fakeAPI) ContainerStart(context.Context, string, container.StartOptions) error {
	return f.startErr
}

func (f *fakeAPI) ContainerKill(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	return nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return f.removeErr
}

func (f *fakeAPI) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return f.containers, nil
}

func (f *fakeAPI) ContainerWait(
	context.Context, string, container.WaitCondition,
) (<-chan container.WaitResponse, <-chan error) {
	status := make(chan container.WaitResponse, 1)
	status <- container.WaitResponse{StatusCode: f.waitStatus}
	return status, make(chan error, 1)
}

func (f *fakeAPI) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(frame(strings.Join(f.execStdout, ""), ""))), nil
}

func (f *fakeAPI) ContainerExecCreate(
	_ context.Context, _ string, opts container.ExecOptions,
) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execOpts = append(f.execOpts, opts)
	return container.ExecCreateResponse{ID: "exec-id"}, nil
}

func (f *fakeAPI) ContainerExecAttach(
	context.Context, string, container.ExecAttachOptions,
) (types.HijackedResponse, error) {
	// A real hijack hands back a net.Conn plus a buffered reader over it, and
	// Close() closes the conn. Both ends of a net.Pipe reproduce that
	// faithfully enough that the cancellation path — close the response to
	// unblock the copier — is genuinely exercised.
	c1, c2 := net.Pipe()
	f.mu.Lock()
	f.pipeEnds = append(f.pipeEnds, c1, c2)
	i := f.execSeq
	f.execSeq++
	stdout, stderr := at(f.execStdout, i), at(f.execStderr, i)
	blocking := f.blockExec
	f.mu.Unlock()

	if blocking {
		return types.HijackedResponse{Conn: c1, Reader: bufio.NewReader(c2)}, nil
	}
	_ = c2.Close()
	return types.HijackedResponse{Conn: c1, Reader: bufio.NewReader(bytes.NewReader(frame(stdout, stderr)))}, nil
}

func (f *fakeAPI) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := 0
	if len(f.execExitCodes) > 0 {
		code = f.execExitCodes[0]
		f.execExitCodes = f.execExitCodes[1:]
	}
	return container.ExecInspect{Running: false, ExitCode: code}, nil
}

func (f *fakeAPI) CopyFromContainer(
	_ context.Context, _, srcPath string,
) (io.ReadCloser, container.PathStat, error) {
	if f.envFileMissing {
		return nil, container.PathStat{}, errdefs.NotFound(errors.New("no such file: " + srcPath))
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "env", Typeflag: tar.TypeReg, Mode: 0o666, Size: int64(len(f.envFileData))})
	_, _ = tw.Write(f.envFileData)
	_ = tw.Close()
	return io.NopCloser(&buf), container.PathStat{}, nil
}

func (f *fakeAPI) CopyToContainer(
	_ context.Context, _, dstPath string, content io.Reader, _ container.CopyToContainerOptions,
) error {
	entries := map[string]string{}
	tr := tar.NewReader(content)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		entries[hdr.Name] = string(body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copiedTo = append(f.copiedTo, copiedFile{dst: dstPath, entries: entries})
	// A truncating copy is what a real daemon would do: the next read sees
	// an empty file.
	f.envFileData = nil
	f.envFileMissing = false
	return nil
}

func (f *fakeAPI) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes = append(f.volumes, opts)
	return volume.Volume{Name: opts.Name}, nil
}

func (f *fakeAPI) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedVolumes = append(f.removedVolumes, name)
	return nil
}

func (f *fakeAPI) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return volume.ListResponse{Volumes: f.listedVolumes}, nil
}

func (f *fakeAPI) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for _, c := range f.pipeEnds {
		_ = c.Close()
	}
	return nil
}

// frame multiplexes stdout and stderr the way the daemon does.
func frame(stdout, stderr string) []byte {
	var buf bytes.Buffer
	if stdout != "" {
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte(stdout))
	}
	if stderr != "" {
		_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stderr).Write([]byte(stderr))
	}
	return buf.Bytes()
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

// newExecutor builds an Executor over a fake daemon with a real workspace
// directory, and returns both.
func newExecutor(t *testing.T, f *fakeAPI, mutate func(*Options)) (*Executor, string) {
	t.Helper()
	ws := t.TempDir()
	opts := Options{Client: f, DefaultImage: "test:latest", User: UserImageDefault}
	if mutate != nil {
		mutate(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, ws
}
