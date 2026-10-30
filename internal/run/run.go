// Package run is the orchestrator: it turns an admitted [protocol.RunRequest]
// into a finished run.
//
// Everything below it is a leaf. internal/registry decides, internal/source
// checks out, internal/secrets resolves, internal/cache mounts,
// internal/executor executes, internal/artifact collects, internal/store
// remembers, internal/logstream narrates. None of them knows about the others.
// This package is the only place they meet, which is why it is also the only
// place the two normative sequences in RUNNER_SPEC live: the ordered gate chain
// of §10 and the state machine of §11.
//
// # The gate chain (§10)
//
// [Orchestrator.Admit] runs gates 1-5 — the ones decidable without touching
// the network or the filesystem — and files the run as queued or rejected.
// [Orchestrator.Execute] runs the rest:
//
//  6. mirror fetch          Source.Fetch
//  7. ref normalization     registry.NormalizeRef
//  9. resolve to SHA        Mirror.Resolve      ┐ swapped by amendment A27
//  8. ref policy            registry.AdmitRef   ┘
//  10. clone + checkout      Mirror.Checkout
//  11. read jobs.yaml        FROM THE WORKSPACE (INV-5)
//  12. job lookup        ┐
//  13. label match       │   Registry.Admit
//  14. input validation  │
//  15. secret names      ┘   then secrets.ResolveAll for the values
//  16. concurrency group     queued — wait, do not fail
//  17. runner disk floor
//  18. cache acquire
//  19. execute
//
// Gate 11 is the invariant the whole design exists to protect. The registry is
// read off disk from the workspace at the resolved commit and never from a
// broker, so a control plane can name a job and cannot define one.
//
// Gates 8 and 9 run in the order shown, which is not the order §10 lists them
// in. Gate 8 is the gate that parks a run for a human, and a park has to name
// the commit being approved or the approval means "this branch, whenever the
// build gets round to it" — which a force-push turns into approval of somebody
// else's code (amendment A27). Resolving first costs a `git rev-parse` against
// an already-fetched mirror.
//
// # The state machine (§11)
//
// Every transition goes through [execution.to], which persists to the store
// before reporting to the broker and never reports a transition the store
// refused. Local truth outranks the broker's view: a broker that is
// unreachable, slow, or lying cannot lose what happened on this machine.
//
// `collecting` always runs once a workspace exists, including after failed,
// timed_out and cancelled, so a broken build still yields its artifacts and
// its logs. Teardown is registered at resource creation and runs from a
// deferred LIFO stack under a context that cancellation cannot reach, so no
// path — not a gate failure, not a cancel, not a panic — leaks a container, a
// cache lock, or a workspace.
package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// DefaultCancelGrace bounds how long the orchestrator waits for a step to stop
// after the run's context is cancelled.
//
// RUNNER_SPEC §11 gives the executor the signalling contract — SIGTERM, 30
// seconds, SIGKILL — so this is that budget plus room for the executor's own
// teardown. It is a backstop against an executor that does not honour the
// contract, not a second implementation of it: dropping the wait to exactly
// 30s would race the SIGKILL the spec asks for, and having no wait at all
// would let a stuck environment hold the run open forever.
const DefaultCancelGrace = 45 * time.Second

// DefaultTeardownTimeout bounds each teardown step. Teardown runs under a
// context cancellation cannot reach — that is the point of it — so it needs a
// deadline of its own or a wedged daemon becomes a wedged agent.
const DefaultTeardownTimeout = 2 * time.Minute

// MaxErrorMsgBytes truncates [protocol.Update.ErrorMsg]. The field crosses to
// a broker and into sqlite; an unbounded one is an unbounded write driven by
// whatever a build printed just before it died.
const MaxErrorMsgBytes = 4096

// Options configures an [Orchestrator]. Everything except Config, Store and
// Runners has a working default or may be nil.
type Options struct {
	// Config supplies the repo allowlist, the TTLs, and the log directory.
	Config *config.Config
	// Store is local truth. Required.
	Store Store
	// Brokers receive reports and log frames — all of them, every time.
	//
	// Empty means local-only operation: state still lands in the store, nothing
	// is published. It is not an error because RUNNER_SPEC §7.3 has the agent
	// doing useful work with no control plane at all. A nil entry is skipped.
	//
	// It is a slice rather than the single broker draft 02 wrote because a
	// machine has two transports and both are entitled to the whole story. It
	// could not be one until amendment A28: with one reported_state and one
	// last_acked_seq for the machine, fanning out would have advanced the remote
	// transport's resume point to everything this machine had emitted, which
	// silently loses exactly the frames covering an outage.
	//
	// Every fan-out here is independent per broker. One transport failing,
	// refusing, or taking thirty seconds to answer must not delay, fail, or
	// alter what the others see; see [reporter] and [execution.openLogs].
	Brokers []broker.Broker

	// AlwaysRedact holds values registered into every run's redactor before the
	// pipeline is built.
	//
	// §16's "redactor contents" lists the machine token, which is not a declared
	// secret and so is registered by nothing in internal/secrets. Registering it
	// is the caller's job because only the agent knows where the token file is;
	// applying it to every run is this package's, because the per-run redactor
	// is created here and INV-4 says registration happens before the first step.
	AlwaysRedact [][]byte
	// Source acquires workspaces. Wrap a *source.Manager with [SourceManager].
	Source Source
	// Secrets resolves the names a job declares. A *secrets.Chain satisfies it.
	Secrets secrets.Provider
	// Caches provisions cache mounts (gate 18).
	Caches cache.Manager
	// Artifacts collects declared outputs. A *artifact.Collector satisfies it.
	Artifacts Collector
	// Runners are the execution slots, in preference order. Gate 13 picks the
	// first whose labels satisfy the job's runs_on.
	Runners []Runner

	// LogDir overrides Config.Paths.Logs.
	LogDir string
	// RingSize is how many lines a `latem logs -f` subscriber can catch up on.
	// Zero uses logstream.DefaultRingSize.
	RingSize int

	// CancelGrace overrides [DefaultCancelGrace].
	CancelGrace time.Duration
	// TeardownTimeout overrides [DefaultTeardownTimeout].
	TeardownTimeout time.Duration
	// ReportAttempts and ReportBackoff bound the retry of a broker report.
	// The run never blocks on either.
	ReportAttempts int
	ReportBackoff  time.Duration

	// Now is the clock. Nil uses time.Now.
	Now func() time.Time

	// Toolchains is what this machine has installed for builds, as the
	// agent last probed it, or nil before it has (A59). A run notes in its
	// log what its job needs that the machine lacks. Nil writes no note.
	Toolchains func() *system.Toolchains
}

// Runner is one execution slot: an executor plus the configuration that
// selects it and bounds what it runs.
type Runner struct {
	Name     string
	Labels   []string
	Executor executor.Executor
	// DefaultImage applies to jobs that do not name their own (docker only).
	DefaultImage string
	Resources    executor.Resources
	// MinFreeGB is gate 17: this runner's own floor, re-checked after gate 13
	// has selected it. Gate 5 could only enforce the weakest floor on the
	// machine, because there was no runner yet to ask.
	MinFreeGB float64
}

// NewRunner adapts a [config.Runner] and its executor into a [Runner].
//
// It reads the disk floor and the resource bounds through the executor-specific
// block rather than a flattened field, so a tart runner cannot pick up a docker
// floor by reaching into the wrong struct.
func NewRunner(c *config.Runner, ex executor.Executor) Runner {
	r := Runner{
		Name:      c.Name,
		Labels:    append([]string(nil), c.Labels...),
		Executor:  ex,
		MinFreeGB: c.MinFreeGB(),
	}
	if c.Docker != nil {
		r.DefaultImage = c.Docker.DefaultImage
		r.Resources = executor.Resources{
			CPUs:   c.Docker.CPUs,
			Memory: c.Docker.Memory,
			GPU:    c.Docker.GPU,
		}
	}
	return r
}

// Orchestrator drives the §10 gate chain and the §11 state machine.
//
// One serves the whole agent. It is safe for concurrent use: several runs
// execute at once, and [Orchestrator.Cancel] may arrive for any of them at any
// moment from a different goroutine.
type Orchestrator struct {
	cfg *config.Config
	// liveCfg is the config a reload last applied (A54); [Orchestrator.config]
	// reads it. cfg stays the one the agent started with.
	liveCfg      atomic.Pointer[config.Config]
	store        Store
	brks         []broker.Broker
	alwaysRedact [][]byte
	src          Source
	secrets      secrets.Provider
	caches       cache.Manager
	artifacts    Collector
	runners      []Runner

	logDir   string
	ringSize int

	cancelGrace     time.Duration
	teardownTimeout time.Duration
	now             func() time.Time
	toolchains      func() *system.Toolchains

	reporter *reporter
	groups   *groups

	mu   sync.Mutex
	live map[string]*handle
}

// New validates the options and builds an orchestrator.
func New(opts Options) (*Orchestrator, error) {
	switch {
	case opts.Config == nil:
		return nil, errors.New("run: New: Config is required")
	case opts.Store == nil:
		return nil, errors.New("run: New: Store is required")
	}

	logDir := opts.LogDir
	if logDir == "" {
		logDir = opts.Config.Paths.Logs
	}
	if logDir == "" {
		return nil, errors.New("run: New: no log directory (set Options.LogDir or Config.Paths.Logs)")
	}

	// A nil entry would be a nil-interface call on every report. Filtering here
	// rather than at each call site keeps the fan-out loops unconditional.
	brks := make([]broker.Broker, 0, len(opts.Brokers))
	for _, b := range opts.Brokers {
		if b != nil {
			brks = append(brks, b)
		}
	}

	o := &Orchestrator{
		cfg:             opts.Config,
		store:           opts.Store,
		brks:            brks,
		alwaysRedact:    opts.AlwaysRedact,
		src:             opts.Source,
		secrets:         opts.Secrets,
		caches:          opts.Caches,
		artifacts:       opts.Artifacts,
		runners:         append([]Runner(nil), opts.Runners...),
		logDir:          logDir,
		ringSize:        orDefaultInt(opts.RingSize, logstream.DefaultRingSize),
		cancelGrace:     orDefault(opts.CancelGrace, DefaultCancelGrace),
		teardownTimeout: orDefault(opts.TeardownTimeout, DefaultTeardownTimeout),
		now:             opts.Now,
		toolchains:      opts.Toolchains,
		groups:          newGroups(),
		live:            map[string]*handle{},
	}
	if o.now == nil {
		o.now = time.Now
	}
	o.reporter = newReporter(brks, opts.ReportAttempts, opts.ReportBackoff)
	return o, nil
}

// Close drains the outstanding broker reports. It does not cancel running
// work: shutdown ordering belongs to the agent, which cancels the context it
// handed to [Orchestrator.Execute] and then calls this.
func (o *Orchestrator) Close() error {
	o.reporter.close()
	return nil
}

// Result is what one [Orchestrator.Execute] call produced.
type Result struct {
	RunID     string
	State     protocol.RunState
	ErrorCode protocol.ErrorCode
	ErrorMsg  string
	ExitCode  *int
	Runner    string
	Artifacts []protocol.ArtifactRef

	// Requeued reports that gate 16 sent the run back to the queue because its
	// concurrency group was occupied — "queued, not rejected". Nothing ran and
	// the caller should claim it again later.
	Requeued bool
}

// SetConfig makes c the config the next admission reads: the repo allowlist
// and each repo's refs and vars, [label_aliases] and [actions] (§8, A54). A
// run already admitted keeps what it was admitted under. The agent calls this
// on a reload, with a config whose restart-only fields — the runners, the
// paths — are the ones this orchestrator was built with.
func (o *Orchestrator) SetConfig(c *config.Config) {
	if c != nil {
		o.liveCfg.Store(c)
	}
}

// config is the live config: the last one [Orchestrator.SetConfig] gave, or
// the one the orchestrator was built with. Read once per decision rather than
// field by field, so one admission never sees half of each.
//
// A stored config is never edited: a reload builds a new one and publishes it
// here, so a run holding the previous one keeps a consistent view of it. What
// is restart-only — Paths above all, read straight from o.cfg by actionsDir,
// toolCacheDir and actionCache — is the startup config's, which no reload
// replaces and config.Reloader carries forward unchanged.
func (o *Orchestrator) config() *config.Config {
	if c := o.liveCfg.Load(); c != nil {
		return c
	}
	return o.cfg
}

// policy adapts the config allowlist to what gates 1-5 consult.
func (o *Orchestrator) policy(ctx context.Context) registry.Policy {
	cfg := o.config()
	p := registry.Policy{
		Repos:     make(map[string]registry.RepoPolicy, len(cfg.Repos)),
		MinFreeGB: cfg.WeakestMinFreeGB(),
	}
	for name, r := range cfg.Repos {
		p.Repos[name] = registry.RepoPolicy{
			AllowedRefs:              r.AllowedRefs,
			RequireApprovalOtherwise: r.RequireApprovalOtherwise,
		}
	}
	// A cache manager that cannot answer leaves FreeGB at zero, which would
	// reject every run against a non-zero floor. Gate 5 is a guard against a
	// full disk, not against an unconfigured one, so an unanswerable question
	// drops the gate rather than closing it — gate 17 asks again once a runner
	// has been chosen.
	if o.caches != nil {
		if free, err := o.caches.FreeGB(ctx); err == nil {
			p.FreeGB = free
		} else {
			p.MinFreeGB = 0
		}
	} else {
		p.MinFreeGB = 0
	}
	return p
}

// repoPolicy returns the policy for one repo, and whether it is allowlisted.
func (o *Orchestrator) repoPolicy(name string) (registry.RepoPolicy, *config.Repo, bool) {
	r, ok := o.config().Repos[name]
	if !ok {
		return registry.RepoPolicy{}, nil, false
	}
	return registry.RepoPolicy{
		AllowedRefs:              r.AllowedRefs,
		RequireApprovalOtherwise: r.RequireApprovalOtherwise,
	}, r, true
}

// approvalTTL and queueTTL are the deadlines a waiting state carries (§11).
func (o *Orchestrator) approvalTTL(repo *config.Repo) time.Duration {
	if repo != nil && repo.ApprovalTTL.Duration() > 0 {
		return repo.ApprovalTTL.Duration()
	}
	return config.DefaultApprovalTTL
}

// approvalTTLFor is [Orchestrator.approvalTTL] by repo name, for the paths
// that have the name and not the config.
func (o *Orchestrator) approvalTTLFor(repo string) time.Duration {
	_, cfg, _ := o.repoPolicy(repo)
	return o.approvalTTL(cfg)
}

func (o *Orchestrator) queueTTL() time.Duration {
	if d := o.config().Agent.QueueTTL.Duration(); d > 0 {
		return d
	}
	return config.DefaultQueueTTL
}

// selectRunner is gate 13's half that [registry.Registry.Admit] cannot do: it
// has one label set, and a machine has several runners.
//
// The first runner carrying every label the job asks for wins, so config order
// is preference order. A nil result means no runner matches, and the caller
// passes an empty label set to Admit so that the refusal comes back as a
// proper E_NO_RUNNER decision listing what was missing — with E_UNKNOWN_JOB
// still taking precedence, because Admit checks gate 12 first.
func (o *Orchestrator) selectRunner(reg *registry.Registry, jobName string) *Runner {
	job, ok := reg.Jobs[jobName]
	if !ok {
		return nil
	}
	// A44: a job with a container step also needs a particular executor. The
	// first runner satisfying both wins. Failing that, the first whose labels
	// match is returned anyway, so that gate 13 refuses with the executor
	// named rather than with a list of labels that were all present.
	need := job.RequiredExecutor()
	var labelsOnly *Runner
	for i := range o.runners {
		if !hasAllLabels(o.runners[i].Labels, job.RunsOn) {
			continue
		}
		suits := (need == "" || executorName(&o.runners[i]) == need) &&
			(!job.RequiresIsolation || executorIsolates(&o.runners[i]))
		if suits {
			return &o.runners[i]
		}
		if labelsOnly == nil {
			labelsOnly = &o.runners[i]
		}
	}
	return labelsOnly
}

// executorName is the runner's executor kind, or "" when it has none.
func executorName(r *Runner) string {
	if r == nil || r.Executor == nil {
		return ""
	}
	return r.Executor.Name()
}

// executorIsolates reports whether a runner's executor confines a step to the
// workspace and the caches it declared (A47).
//
// Allow-listed by name rather than asked of the executor, and deliberately:
// this is the answer gate 13 uses to decide whether third-party code may run
// on a machine, so a new executor should have to be added here by somebody
// who has thought about it rather than inherit a default. An unknown executor
// does not isolate.
func executorIsolates(r *Runner) bool {
	switch executorName(r) {
	case "docker", "tart":
		return true
	default:
		return false
	}
}

func hasAllLabels(have, required []string) bool {
	set := make(map[string]bool, len(have))
	for _, l := range have {
		set[l] = true
	}
	for _, r := range required {
		if !set[r] {
			return false
		}
	}
	return true
}

// RunnerNames lists the configured slots, for store.ClaimNext.
func (o *Orchestrator) RunnerNames() []string {
	out := make([]string, 0, len(o.runners))
	for _, r := range o.runners {
		out = append(out, r.Name)
	}
	return out
}

func orDefault(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// truncate bounds a message that is about to be persisted or transmitted.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf(" … (%d bytes truncated)", len(s)-max)
}

// actionPolicy is what this machine has decided about third-party actions.
//
// Read from config on every run rather than cached at construction, so that a
// reload (§8) takes effect on the next build instead of on the next restart —
// an operator who has just narrowed an allowlist because of something they
// read should not have to remember that.
// labelAliases is the machine's [label_aliases] (A50), nil without a config.
func (o *Orchestrator) labelAliases() map[string][]string {
	cfg := o.config()
	if cfg == nil {
		return nil
	}
	return cfg.LabelAliases
}

func (o *Orchestrator) actionPolicy() actions.Policy {
	cfg := o.config()
	if cfg == nil {
		return actions.DefaultPolicy()
	}
	return actions.Policy{
		Allowed:         cfg.Actions.Allowed,
		RequirePinned:   cfg.Actions.PinningRequired(),
		AllowUnisolated: cfg.Actions.AllowUnisolated,
	}
}

// actionCache is where fetched actions live, or nil on a machine with no
// config directory — in which case a remote action is refused by name rather
// than fetched into somewhere arbitrary.
func (o *Orchestrator) actionCache() workflow.RemoteFetcher {
	if o.cfg == nil || o.cfg.Paths.Actions == "" {
		return nil
	}
	return &actions.Cache{
		Root:   o.cfg.Paths.Actions,
		Policy: o.actionPolicy(),
		Git:    actions.GitRunner{},
	}
}
