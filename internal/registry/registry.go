// Package registry parses .latem/jobs.yaml and decides what may run.
//
// This is the correctness core of the system and it deliberately touches
// nothing: no network, no filesystem, no processes. Every function here is
// pure, which is why it can carry the hostile-input table from RUNNER_SPEC §5
// without any infrastructure to run against.
//
// The registry is read from the checked-out workspace at the resolved commit
// and never from a broker (INV-5). A broker supplies a job *name*; this
// package is what turns that name into something executable, or refuses to.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// SchemaVersion is the only supported value of the top-level `version` key.
const SchemaVersion = 1

// Path is where the registry lives inside a repository.
const Path = ".latem/jobs.yaml"

// Defaults applied when a job does not override them.
const (
	DefaultTimeout = 30 * time.Minute
	DefaultShell   = "bash"
)

// Limits that bound a registry. These are not tuning knobs; they exist so a
// malformed or hostile file fails at parse rather than at some later point
// where the failure is harder to attribute.
const (
	MaxJobs          = 256
	MaxStepsPerJob   = 128
	MaxInputsPerJob  = 32
	MaxSecretsPerJob = 32
	MaxRunLength     = 64 * 1024
)

var (
	// jobNamePattern also bounds what can appear in `latem run <repo> <job>`.
	jobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// identPattern shapes input names, env keys, and secret names. All three
	// become environment variables, so they share the C identifier rules.
	identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// cacheNamePattern is stricter because the name becomes part of a Docker
	// volume name: latem-cache-<runner>-<cache>.
	cacheNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// labelPattern shapes runs_on entries and runner labels.
	labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Registry is a parsed and validated jobs.yaml.
type Registry struct {
	Version  int             `json:"version"`
	Defaults Defaults        `json:"defaults,omitempty"`
	Caches   []CacheDecl     `json:"caches,omitempty"`
	Jobs     map[string]*Job `json:"jobs"`
}

// Defaults are registry-wide fallbacks.
type Defaults struct {
	Timeout protocol.Duration `json:"timeout,omitempty"`
	Shell   string            `json:"shell,omitempty"`
}

// CacheDecl declares a cache and where it mounts inside the environment.
type CacheDecl struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Job is one named unit of work.
type Job struct {
	Description string            `json:"description,omitempty"`
	RunsOn      []string          `json:"runs_on"`
	Image       string            `json:"image,omitempty"`
	Timeout     protocol.Duration `json:"timeout,omitempty"`
	Caches      []string          `json:"caches,omitempty"`
	// ConcurrencyGroup serialises across jobs, not merely across runners. At
	// most one run per group per machine; others queue rather than fail.
	ConcurrencyGroup string            `json:"concurrency_group,omitempty"`
	Inputs           map[string]*Input `json:"inputs,omitempty"`
	// RequiresIsolation marks a job whose steps came from code the operator
	// did not write — a remote action (A42 phase 5). It is set by the
	// compiler and excluded from decoding: by the time a job reaches here the
	// action has been inlined into ordinary steps, so nothing downstream can
	// tell they were third-party unless the compiler says so.
	//
	// Whether an operator has opted out of the requirement is decided where
	// the policy lives, at compile time; this field is only ever the answer.
	RequiresIsolation bool `json:"-"`
	// CacheDecls are caches this job declared for itself — `uses:
	// actions/cache`, compiled into a mount (A49). A registry, not a job, is
	// where caches are declared, so the resolver lifts these into
	// [Registry.Caches]; they ride here only because a compiler produces a job
	// before anything produces a registry. Excluded from decoding.
	CacheDecls []CacheDecl    `json:"-"`
	Secrets    []string       `json:"secrets,omitempty"`
	Artifacts  []ArtifactDecl `json:"artifacts,omitempty"`
	Steps      []Step         `json:"steps"`
}

// Step is one command.
type Step struct {
	Name string `json:"name,omitempty"`
	// ID names the step for later steps' late bindings to refer to. It is
	// excluded from decoding: the native format has no use for it yet, and
	// the compiler is the only writer. Adding it to jobs.yaml would be a §9
	// change and should arrive as one.
	ID string `json:"-"`
	// Run is literal. This is the only place in the entire system where a
	// command may be written, and interpolation into it is a parse error —
	// see [validateInterpolation].
	Run string `json:"run"`
	// Env may reference ${input.NAME}. It may not reference secrets, which
	// falls out of the syntax rather than needing its own check: ${input.NAME}
	// is the only legal expansion, so ${secret.X} is a parse error.
	Env     map[string]string `json:"env,omitempty"`
	Timeout protocol.Duration `json:"timeout,omitempty"`

	// Shell is GitHub's shell for a step compiled from a workflow — "github",
	// "bash" or "sh", as executor.Shell defines them — and empty for a
	// jobs.yaml step, which keeps §9's (A50). Excluded from decoding: only the
	// compiler writes it.
	Shell string `json:"-"`

	// Late holds env values that are computed as the step is about to run,
	// and If is a condition evaluated then. Both are [LateValue]; see that
	// type for why they exist and why they are excluded from decoding. A
	// jobs.yaml that tries to write either is a strict-decoding error, which
	// is the intended answer.
	Late map[string]LateValue `json:"-"`
	If   Condition            `json:"-"`

	// Container makes this step a container instead of a command (A44). It is
	// exclusive with Run, and excluded from decoding; see [ContainerStep].
	Container *ContainerStep `json:"-"`
	// Artifact makes this step a collection the run loop performs itself
	// (A49). Exclusive with Run and Container; see [ArtifactStep].
	Artifact *ArtifactStep `json:"-"`
}

// ArtifactDecl is a glob relative to the workspace root, plus retention.
type ArtifactDecl struct {
	Path   string            `json:"path"`
	Retain protocol.Duration `json:"retain,omitempty"`
}

// Parse decodes and validates a registry.
//
// Decoding is strict: an unknown field is an error, not a warning. A typo in a
// CI file should fail the run that introduced it rather than silently doing
// something other than what was written.
func Parse(data []byte) (*Registry, error) {
	var r Registry
	if err := yaml.UnmarshalStrict(data, &r); err != nil {
		return nil, &Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("parsing %s", Path),
			Err:  err,
		}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks every rule in RUNNER_SPEC §9.
//
// Problems are collected rather than returned one at a time. Someone fixing a
// jobs.yaml should see everything wrong with it in one pass, not discover the
// next error after each round trip through a build.
func (r *Registry) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if r.Version != SchemaVersion {
		add("version must be %d, got %d", SchemaVersion, r.Version)
	}

	if r.Defaults.Shell == "" {
		r.Defaults.Shell = DefaultShell
	}
	if r.Defaults.Timeout == 0 {
		r.Defaults.Timeout = protocol.Duration(DefaultTimeout)
	}
	if r.Defaults.Timeout < 0 {
		add("defaults.timeout must be positive")
	}

	declaredCaches := map[string]bool{}
	for i, c := range r.Caches {
		switch {
		case c.Name == "":
			add("caches[%d]: name is required", i)
		case !cacheNamePattern.MatchString(c.Name):
			add("caches[%d]: name %q must match %s", i, c.Name, cacheNamePattern)
		case declaredCaches[c.Name]:
			add("caches[%d]: duplicate cache name %q", i, c.Name)
		default:
			declaredCaches[c.Name] = true
		}
		if c.Path == "" {
			add("caches[%d] (%s): path is required", i, c.Name)
		}
	}

	if len(r.Jobs) == 0 {
		add("at least one job is required")
	}
	if len(r.Jobs) > MaxJobs {
		add("too many jobs: %d, limit %d", len(r.Jobs), MaxJobs)
	}

	// Sorted so the error list is stable across runs; map order otherwise
	// reshuffles the output of an unchanged file.
	for _, name := range sortedKeys(r.Jobs) {
		for _, err := range r.Jobs[name].validate(name, declaredCaches) {
			problems = append(problems, fmt.Errorf("job %q: %w", name, err))
		}
	}

	if len(problems) > 0 {
		return &Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("%s is invalid", Path),
			Err:  errors.Join(problems...),
		}
	}
	return nil
}

func (j *Job) validate(name string, declaredCaches map[string]bool) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if !jobNamePattern.MatchString(name) {
		add("name must match %s", jobNamePattern)
	}

	if len(j.RunsOn) == 0 {
		add("runs_on is required — a job with no labels could land on any runner")
	}
	for _, l := range j.RunsOn {
		if !labelPattern.MatchString(l) {
			add("runs_on: label %q must match %s", l, labelPattern)
		}
	}

	if j.Timeout < 0 {
		add("timeout must be positive")
	}

	for _, c := range j.Caches {
		if !declaredCaches[c] {
			add("caches: %q is not declared in the top-level caches block", c)
		}
	}

	if j.ConcurrencyGroup != "" && !cacheNamePattern.MatchString(j.ConcurrencyGroup) {
		add("concurrency_group %q must match %s", j.ConcurrencyGroup, cacheNamePattern)
	}

	if len(j.Inputs) > MaxInputsPerJob {
		add("too many inputs: %d, limit %d", len(j.Inputs), MaxInputsPerJob)
	}
	for _, in := range sortedKeys(j.Inputs) {
		if !identPattern.MatchString(in) {
			add("inputs: name %q must match %s", in, identPattern)
		}
		if err := j.Inputs[in].validate(); err != nil {
			add("inputs.%s: %w", in, err)
		}
	}

	if len(j.Secrets) > MaxSecretsPerJob {
		add("too many secrets: %d, limit %d", len(j.Secrets), MaxSecretsPerJob)
	}
	seenSecret := map[string]bool{}
	for _, s := range j.Secrets {
		if !identPattern.MatchString(s) {
			add("secrets: name %q must match %s", s, identPattern)
		}
		if seenSecret[s] {
			add("secrets: %q declared twice", s)
		}
		seenSecret[s] = true
	}

	for i, a := range j.Artifacts {
		if err := a.validate(); err != nil {
			add("artifacts[%d]: %w", i, err)
		}
	}

	switch {
	case len(j.Steps) == 0:
		add("at least one step is required")
	case len(j.Steps) > MaxStepsPerJob:
		add("too many steps: %d, limit %d", len(j.Steps), MaxStepsPerJob)
	}
	// earlier accumulates the ids of steps already seen, so a late binding
	// can be checked against exactly the steps that will have run by then.
	earlier := map[string]bool{}
	for i, s := range j.Steps {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("%d", i)
		}
		for _, err := range s.validate(j.Inputs) {
			add("steps[%s]: %w", label, err)
		}
		for _, err := range validateLate(s, earlier) {
			add("steps[%s]: %w", label, err)
		}
		if s.ID != "" {
			if earlier[s.ID] {
				add("steps[%s]: id %q is already used by an earlier step", label, s.ID)
			}
			earlier[s.ID] = true
		}
	}

	return problems
}

func (s Step) validate(inputs map[string]*Input) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	kinds := 0
	for _, set := range []bool{strings.TrimSpace(s.Run) != "", s.Container != nil, s.Artifact != nil} {
		if set {
			kinds++
		}
	}
	switch {
	case kinds > 1:
		// Exactly one. A step that is two things would have to pick, and
		// whichever it picked would be the wrong one for somebody.
		add("a step is a command, a container or an artifact, and only one")
	case s.Artifact != nil:
		problems = append(problems, s.Artifact.validate()...)
	case s.Container != nil:
		problems = append(problems, s.Container.validate()...)
	case strings.TrimSpace(s.Run) == "":
		add("run is required")
	case len(s.Run) > MaxRunLength:
		add("run is %d bytes, limit %d", len(s.Run), MaxRunLength)
	}

	// Interpolation is legal only in env values. A ${...} in run is rejected
	// rather than passed through, because a job author who writes one expects
	// substitution and would otherwise get a literal string in a shell command
	// — the exact confusion that leads people to reach for eval.
	if err := rejectInterpolation(s.Run); err != nil {
		add("run: %w", err)
	}

	if s.Timeout < 0 {
		add("timeout must be positive")
	}

	for _, k := range sortedKeys(s.Env) {
		if !identPattern.MatchString(k) {
			add("env: key %q must match %s", k, identPattern)
		}
		if strings.HasPrefix(k, "LATEM_") {
			add("env: key %q is reserved — LATEM_* is the agent's namespace", k)
		}
		if err := validateInterpolation(s.Env[k], inputs); err != nil {
			add("env.%s: %w", k, err)
		}
	}

	return problems
}

func (a ArtifactDecl) validate() error {
	switch {
	case a.Path == "":
		return errors.New("path is required")
	case strings.HasPrefix(a.Path, "/"):
		return fmt.Errorf("path %q must be relative to the workspace root", a.Path)
	case strings.Contains(a.Path, ".."):
		return fmt.Errorf("path %q must not contain %q", a.Path, "..")
	case a.Retain < 0:
		return errors.New("retain must be positive")
	}
	return nil
}

// Cache returns the declaration for a name.
func (r *Registry) Cache(name string) (CacheDecl, bool) {
	for _, c := range r.Caches {
		if c.Name == name {
			return c, true
		}
	}
	return CacheDecl{}, false
}

// JobTimeout resolves a job's wall clock against the registry defaults.
func (r *Registry) JobTimeout(j *Job) time.Duration {
	if j.Timeout > 0 {
		return j.Timeout.Duration()
	}
	if r.Defaults.Timeout > 0 {
		return r.Defaults.Timeout.Duration()
	}
	return DefaultTimeout
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
