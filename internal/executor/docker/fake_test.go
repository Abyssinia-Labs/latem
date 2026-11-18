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
	"github.com/docker/docker/api/types/system"
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
	// archives, when set, is what CopyFromContainer answers, by path; a path
	// not in it is not found.
	archives      map[string][]byte
	containers    []container.Summary
	pingErr       error
	waitStatus    int64
	listedVolumes []*volume.Volume
	repoDigests   []string

	// A109: the tool cache. images, when set, is every image the daemon has,
	// by reference or id; otherwise every reference is one image, imageID.
	// volumeCreatedAt is what VolumeCreate reports. probeLogs is what each
	// probe container prints, one per probe, before falling back to
	// execStdout. volumeRemoveErrs fails VolumeRemove by name.
	images           map[string]image.InspectResponse
	imageID          string
	volumeCreatedAt  string
	probeLogs        []string
	probeLogsRead    int
	volumeRemoveErrs map[string]error

	// LAT-23: the readiness exec Prepare runs before any step. It is kept
	// out of execOpts and the per-exec queues above, so a test about steps
	// sees only steps. readyFailures is how many readiness execs the engine
	// answers as Kiri did, with no /bin/sh; readyNever answers every one so.
	readyFailures int
	readyNever    bool
	readyExecs    []container.ExecOptions
	readyFailed   bool // the last readiness exec failed; read by its inspect
	// inspectStates is what ContainerInspect answers, one per call, the last
	// repeated. Empty means running with a pid.
	inspectStates []container.State
	inspects      int

	execSeq  int
	closed   bool
	pipeEnds []net.Conn
}

type copiedFile struct {
	dst     string
	entries map[string]string
	// headers are the archive's own, for what a copy says of mode and owner.
	headers map[string]tar.Header
}

func (f *fakeAPI) Info(context.Context) (system.Info, error) {
	return system.Info{ID: "FAKE"}, nil
}

func (f *fakeAPI) Ping(context.Context) (types.Ping, error) {
	return types.Ping{APIVersion: "1.49"}, f.pingErr
}

func (f *fakeAPI) ImageInspect(_ context.Context, ref string) (image.InspectResponse, error) {
	if f.imageMissing {
		return image.InspectResponse{}, errdefs.NotFound(errors.New("no such image: " + ref))
	}
	if f.images != nil {
		img, ok := f.images[ref]
		if !ok {
			return image.InspectResponse{}, errdefs.NotFound(errors.New("no such image: " + ref))
		}
		return img, nil
	}
	id := f.imageID
	if id == "" {
		id = fakeImageID
	}
	return image.InspectResponse{ID: id, RepoTags: []string{ref}, RepoDigests: f.repoDigests}, nil
}

// fakeImageID is the image every reference names when a test sets none.
const fakeImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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

func (f *fakeAPI) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := container.State{Status: container.StateRunning, Running: true, Pid: 42}
	if n := len(f.inspectStates); n > 0 {
		st = f.inspectStates[min(f.inspects, n-1)]
	}
	f.inspects++
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: &st}}, nil
}

// kiriShellMissing is what Kiri 0.2.1 printed for an exec that came too soon
// after the start (LAT-23).
const kiriShellMissing = `kiri: cannot start container: exec: "/bin/sh": stat /bin/sh: no such file or directory` + "\n"

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
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probeLogsRead < len(f.probeLogs) {
		out := f.probeLogs[f.probeLogsRead]
		f.probeLogsRead++
		return io.NopCloser(bytes.NewReader(frame(out, ""))), nil
	}
	return io.NopCloser(bytes.NewReader(frame(strings.Join(f.execStdout, ""), ""))), nil
}

func (f *fakeAPI) ContainerExecCreate(
	_ context.Context, _ string, opts container.ExecOptions,
) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if isReadyExec(opts.Cmd) {
		f.readyExecs = append(f.readyExecs, opts)
		return container.ExecCreateResponse{ID: "ready-exec-id"}, nil
	}
	f.execOpts = append(f.execOpts, opts)
	return container.ExecCreateResponse{ID: "exec-id"}, nil
}

func (f *fakeAPI) ContainerExecAttach(
	_ context.Context, execID string, _ container.ExecAttachOptions,
) (types.HijackedResponse, error) {
	if execID == "ready-exec-id" {
		f.mu.Lock()
		f.readyFailed = f.readyNever || len(f.readyExecs) <= f.readyFailures
		stderr := ""
		if f.readyFailed {
			stderr = kiriShellMissing
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		f.pipeEnds = append(f.pipeEnds, c1)
		f.mu.Unlock()
		return types.HijackedResponse{Conn: c1, Reader: bufio.NewReader(bytes.NewReader(frame("", stderr)))}, nil
	}
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
	// blockExec models a step that never finishes. The exec that *stops* a
	// step (A45) runs as root and must not hang behind the thing it is
	// stopping, or every timeout test would wait out the grace period.
	isStop := i < len(f.execOpts) && f.execOpts[i].User == "0"
	blocking := f.blockExec && !isStop
	f.mu.Unlock()

	if blocking {
		return types.HijackedResponse{Conn: c1, Reader: bufio.NewReader(c2)}, nil
	}
	_ = c2.Close()
	return types.HijackedResponse{Conn: c1, Reader: bufio.NewReader(bytes.NewReader(frame(stdout, stderr)))}, nil
}

func (f *fakeAPI) ContainerExecInspect(_ context.Context, execID string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if execID == "ready-exec-id" {
		if f.readyFailed {
			return container.ExecInspect{ExitCode: 127}, nil
		}
		return container.ExecInspect{}, nil
	}
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
	if f.archives != nil {
		// A84: what the container's runner.temp holds, as the daemon would
		// archive it.
		data, ok := f.archives[srcPath]
		if !ok {
			return nil, container.PathStat{}, errdefs.NotFound(errors.New("no such file: " + srcPath))
		}
		return io.NopCloser(bytes.NewReader(data)), container.PathStat{}, nil
	}
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
	entries, headers := map[string]string{}, map[string]tar.Header{}
	tr := tar.NewReader(content)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		entries[hdr.Name] = string(body)
		headers[hdr.Name] = *hdr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copiedTo = append(f.copiedTo, copiedFile{dst: dstPath, entries: entries, headers: headers})
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
	return volume.Volume{Name: opts.Name, Labels: opts.Labels, CreatedAt: f.volumeCreatedAt}, nil
}

func (f *fakeAPI) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.volumeRemoveErrs[name]; err != nil {
		return err
	}
	f.removedVolumes = append(f.removedVolumes, name)
	return nil
}

// VolumeList answers listedVolumes, filtered as the daemon filters: every
// label filter must match, as key or key=value, and a name filter is a
// substring.
func (f *fakeAPI) VolumeList(_ context.Context, opts volume.ListOptions) (volume.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*volume.Volume
	for _, v := range f.listedVolumes {
		if v == nil || volumeMatches(v, opts) {
			out = append(out, v)
		}
	}
	return volume.ListResponse{Volumes: out}, nil
}

func volumeMatches(v *volume.Volume, opts volume.ListOptions) bool {
	for _, l := range opts.Filters.Get("label") {
		key, want, hasValue := strings.Cut(l, "=")
		got, ok := v.Labels[key]
		if !ok || (hasValue && got != want) {
			return false
		}
	}
	for _, n := range opts.Filters.Get("name") {
		if !strings.Contains(v.Name, n) {
			return false
		}
	}
	return true
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
