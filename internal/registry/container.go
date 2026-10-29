package registry

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ContainerStep is a step that is a container rather than a command (A44).
//
// It exists for `uses: docker://…`, and the compiler in internal/workflow is
// its only writer: the field that holds it is excluded from decoding. A native
// `container:` step is plausibly a real §9 feature, and if it arrives it
// should arrive as one rather than as a side effect of Actions compatibility.
//
// Args is an argv, not a command line. Each element reaches the container as
// one argument whatever it contains, which is INV-3's discipline with nothing
// added: a value of `; rm -rf /` is an argument.
type ContainerStep struct {
	Image      string
	Entrypoint string
	Args       []string
}

// ExecutorDocker is the executor a container step needs.
const ExecutorDocker = "docker"

const (
	maxContainerArgs     = 256
	maxContainerArgBytes = 32 << 10
)

// imagePattern is deliberately narrower than the OCI reference grammar. It
// admits registry/name:tag and name@sha256:…, and refuses whitespace, control
// characters and a leading dash — the shapes that are only ever a mistake or
// an attempt.
var imagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)

// Pinned reports whether the image is named by digest. A tag is a mutable
// reference: INV-5 yields reproducibility only for a digest, and A44 says so
// rather than leaving it to be discovered.
func (c *ContainerStep) Pinned() bool { return strings.Contains(c.Image, "@sha256:") }

func (c *ContainerStep) validate() []error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	if !imagePattern.MatchString(c.Image) {
		add("container: image %q is not a reference of the form name[:tag] or name@sha256:…", c.Image)
	}
	if strings.ContainsAny(c.Entrypoint, "\x00\n\r") {
		add("container: entrypoint contains a control character")
	}
	if len(c.Args) > maxContainerArgs {
		add("container: %d args, limit %d", len(c.Args), maxContainerArgs)
	}
	for i, a := range c.Args {
		if len(a) > maxContainerArgBytes {
			add("container: args[%d] is %d bytes, limit %d", i, len(a), maxContainerArgBytes)
		}
		if strings.ContainsRune(a, 0) {
			add("container: args[%d] contains a NUL", i)
		}
	}
	return problems
}

// RequiredExecutor names the executor a job cannot run without, or "".
//
// Derived from the steps rather than stored beside them, so that it cannot
// drift: a job requires docker exactly when one of its steps is a container.
func (j *Job) RequiredExecutor() string {
	for _, s := range j.Steps {
		if s.Container != nil {
			return ExecutorDocker
		}
	}
	return ""
}

// Runner is what admission needs to know about the runner a job would land on.
type Runner struct {
	Labels []string
	// Isolates reports whether this executor confines a step to the workspace
	// and the caches it declared. Docker does. Native does not, on any OS —
	// a step runs as the agent's user with everything that user can reach,
	// and that is a property of running on the host rather than a gap waiting
	// on a VM (A47).
	Isolates bool
	// Executor is "docker", "native" or "tart". Empty means the caller did not
	// say, and a job that requires an executor is refused on it: the gate
	// fails closed, because the alternative is a container step reaching an
	// executor that has no containers.
	Executor string
}

// ArtifactStep is a step the run loop performs itself rather than handing to
// an executor: collecting files into this run's artifacts, or copying them
// back out (A49).
//
// It exists for actions/upload-artifact and actions/download-artifact, and the
// compiler is its only writer — the field holding it is excluded from
// decoding. It is a step rather than an end-of-job declaration because the
// step's `if:` decides whether it happens, and upload steps are usually
// `if: always()` or `if: failure()`; a condition is evaluated per step, so the
// collection has to be one.
type ArtifactStep struct {
	// Download is false for an upload.
	Download bool
	// Name is the artifact's name, and the directory it lives in. Empty on a
	// download means every artifact this run has.
	Name string

	// Upload: patterns relative to the workspace, what to leave out, and how
	// long to keep it. IfNoFilesFound is "warn", "error" or "ignore".
	Include        []string
	Exclude        []string
	Retain         protocol.Duration
	IfNoFilesFound string

	// Download: where in the workspace to put it. Empty is the root.
	Path string
}

var ifNoFilesFound = map[string]bool{"warn": true, "error": true, "ignore": true}

func (a *ArtifactStep) validate() []error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	if a.Name != "" && !artifactNamePattern.MatchString(a.Name) {
		add("artifact: name %q is not a plain name", a.Name)
	}
	if strings.Contains(a.Name, "..") {
		add("artifact: name %q contains ..", a.Name)
	}
	if a.Download {
		if len(a.Include) > 0 || len(a.Exclude) > 0 {
			add("artifact: a download has no patterns")
		}
		if bad := escapes(a.Path); bad != "" {
			add("artifact: download path %q %s", a.Path, bad)
		}
		return problems
	}
	if a.Name == "" {
		add("artifact: an upload needs a name")
	}
	if len(a.Include) == 0 {
		add("artifact: an upload needs at least one path")
	}
	for _, p := range append(append([]string(nil), a.Include...), a.Exclude...) {
		if bad := escapes(p); bad != "" {
			add("artifact: path %q %s", p, bad)
		}
	}
	if !ifNoFilesFound[a.IfNoFilesFound] {
		add("artifact: if-no-files-found %q is not warn, error or ignore", a.IfNoFilesFound)
	}
	if a.Retain < 0 {
		add("artifact: retention must be positive")
	}
	return problems
}

var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,99}$`)

// escapes answers why a path cannot be one in the workspace, or "". The
// collector re-checks all of this against the filesystem at run time; this is
// the part that can be refused before anything runs.
func escapes(p string) string {
	switch {
	case strings.HasPrefix(p, "/"):
		return "is absolute, and artifacts are collected from the workspace"
	case strings.HasPrefix(p, "~"):
		return "starts with ~, and artifacts are collected from the workspace"
	case strings.ContainsRune(p, 0):
		return "contains a NUL"
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "climbs out of the workspace"
		}
	}
	return ""
}
