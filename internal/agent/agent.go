// Package agent is the supervisor: the process `latem agent` starts, and the
// only thing in this tree that owns a machine rather than a run.
//
// Everything below it owns one concern. internal/run drives one run through
// the §10 gate chain and the §11 state machine; internal/broker/local carries
// commands; internal/store remembers; internal/cache, internal/logstream and
// the executors reclaim, narrate, and execute. None of them has a lifetime.
// This package supplies the one thing they all lack — a beginning, a middle,
// and an end — which is why RUNNER_SPEC §6 is written about it and why almost
// every hard problem here is a shutdown or a restart rather than a build.
//
// # The nine steps of §6, and why the order is not negotiable
//
//  1. Load config and fail fast. An agent that starts on a config it could not
//     validate fails later, inside somebody's build, where it is expensive.
//  2. Take the flock on the config directory. Exactly one agent per directory:
//     two agents sharing a work tree, a socket, and a queue is not redundancy,
//     it is two processes corrupting one state directory.
//  3. Open sqlite and migrate — after the lock, so two agents cannot race a
//     schema change.
//  4. Preflight each runner (§20). A runner that cannot execute never accepts
//     work.
//  5. Reconcile. Not optional: see [Agent.Reconcile].
//  6. Start the runner pool.
//  7. Connect brokers — local always, remote when one exists.
//  8. Heartbeat every 15s, including while draining.
//  9. Block until SIGINT or SIGTERM, then drain.
//
// # The doorbell is not the queue
//
// [local.Broker.Commands] deliberately does not replay the backlog. sqlite is
// the truth and the socket is a notification, so a run enqueued while the
// agent was down — or one that arrived and was not claimed because every
// worker was busy — is reachable only through store.ClaimNext. The pool
// therefore polls as well as reacting, and an agent that only waited on
// Commands would sit idle beside a full queue. That is stated at length in
// internal/broker/local's package comment; it is repeated here because this is
// the package that gets it wrong.
//
// internal/broker/remote is the opposite and both are consumed here, so the
// difference has to be understood rather than smoothed over. There is no shared
// database behind that transport, so its Commands *does* replay a backlog after
// every reconnect. What makes reading both safe is that neither handler does
// anything a repeat could damage: a run command rings a doorbell, and a cancel
// is defined by §7.1 as a no-op for an unknown, unclaimed, or terminal id.
//
// # What "no orphans after kill -9" costs
//
// The M4 acceptance criterion is that a hard kill mid-run leaves no container,
// no VM, and no workspace behind once the agent restarts, and that the run
// reads interrupted. Nothing the dying process does can help — SIGKILL runs no
// defer — so the entire guarantee lives in [Agent.Reconcile] at the next
// start, driven by two facts written down before the work began: the run row
// in sqlite, and the `latem.run-id` label on every resource an executor
// creates. Reconciliation is the only code path in the agent that must work on
// a machine where nothing else did.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/broker/remote"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/metrics"
	"github.com/Abyssinia-Labs/latem/internal/run"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Intervals from RUNNER_SPEC §20 and §14.4. Each is overridable through
// [Options] so a test does not have to wait fifteen seconds to observe a
// heartbeat, and none is overridable through config: an operator who could
// lengthen the heartbeat could silently make their machine look offline.
const (
	// DefaultHeartbeatInterval is §20's fifteen seconds. A control plane marks
	// a machine offline after three missed beats.
	DefaultHeartbeatInterval = 15 * time.Second

	// DefaultPreflightInterval is §20's rolling check. Five minutes is long
	// enough that probing Docker is free and short enough that a daemon
	// restart costs one build at most.
	DefaultPreflightInterval = 5 * time.Minute

	// DefaultMaintenanceInterval is the daily ticker: cache prune, log
	// retention, TTL sweep, mirror gc (§14.4, §16, §11).
	DefaultMaintenanceInterval = 24 * time.Hour

	// DefaultToolchainsInterval is how often the agent looks again at what it
	// has for builds (A59, A61). Xcode changes under a running agent — the
	// App Store updates it, an operator installs a second one — and a probe is
	// a second or two of plutil and a JDK list.
	DefaultToolchainsInterval = 15 * time.Minute

	// DefaultClaimInterval is how often a worker asks the queue for work when
	// no doorbell has rung. It is short because the doorbell only covers runs
	// submitted to a live agent: an approval, a re-queue from gate 16, and
	// every run enqueued while the agent was down arrive with no notification
	// at all.
	DefaultClaimInterval = 2 * time.Second

	// DefaultSlotWait bounds how long a run waits for a slot on the runner
	// that gate 13 selected for it. It mirrors the cache lock timeout in
	// §14.3: waiting is normal, waiting forever is a build nobody can explain.
	DefaultSlotWait = 10 * time.Minute

	// DefaultPreflightTimeout bounds one runner's preflight probe. Docker's
	// ping is milliseconds; a hung daemon is not, and a preflight that blocks
	// stalls the availability of every other runner behind it.
	DefaultPreflightTimeout = 60 * time.Second

	// DefaultTeardownGrace is how long shutdown waits for in-flight runs to
	// tear down after their contexts are cancelled, on top of shutdown_grace.
	// It is the executor's SIGTERM → 30s → SIGKILL budget (§6) plus room for
	// the collecting phase that follows it.
	DefaultTeardownGrace = 3 * time.Minute
)

// Options configures an [Agent].
type Options struct {
	// Config is a validated config (§6 step 1). Required: this package does
	// not load it, because `latem agent` must be able to fail on a bad config
	// before anything else in §6 has happened.
	Config *config.Config

	// Version is stamped into MachineHealth and the metrics build-info gauge.
	Version string

	// Logger receives the agent's own narration. Nil discards it, which is
	// what a test wants and never what an operator wants.
	Logger *slog.Logger

	// Once executes a single claimable run and exits. It is a debugging aid
	// (`latem agent --once`): it still performs the whole of §6, so it is a
	// faithful way to reproduce a run under a debugger, and it exits rather
	// than idling when the queue is empty.
	Once bool

	// DrainAfter stops accepting new runs after this long and shuts down when
	// the in-flight ones finish. Zero runs until signalled.
	DrainAfter time.Duration

	// NewExecutor builds the executor behind one runner. Nil uses
	// [DefaultExecutor]. A test substitutes one that needs no daemon.
	//
	// An error here is not fatal to the agent: the runner is registered
	// permanently unavailable with the error as its reason (§20), so a machine
	// whose Docker socket is missing still serves its native runners and still
	// says why the other one is dark.
	NewExecutor func(r *config.Runner, paths config.Paths) (executor.Executor, error)

	// Secrets overrides the §15 resolution chain. Nil builds the standard one
	// from config. A test supplies its own so no real credential store is
	// touched.
	Secrets secrets.Provider

	// Releases is the source §20's self-update reads. Nil builds the real one
	// from the embedded trusted key; a test supplies one that needs no
	// network. It is consulted only when agent.auto_update is true.
	Releases releaseSource

	// Exec replaces this process image after an update. Nil uses syscall.Exec,
	// which does not return — so a test that wants to observe the re-exec
	// rather than become the new binary substitutes its own.
	Exec func(path string, argv, env []string) error

	// BinaryVersion asks a latem binary its version, for SIGUSR2 (A56). Nil
	// runs it with `version --short`.
	BinaryVersion func(ctx context.Context, path string) (string, error)

	// ProbeToolchains finds what this machine has for builds, for the note a
	// run writes about what its job needs and the machine lacks (A59). Nil
	// probes nothing and writes no note.
	// stepPath is the PATH a native step starts from (A60).
	ProbeToolchains func(ctx context.Context, stepPath string) system.Toolchains

	// MeasureMachine is the machine's size, for the capacity a heartbeat
	// carries (A63), measured beside the toolchains and as often. Nil sends
	// none.
	MeasureMachine func(ctx context.Context) capacity.Machine

	// Intervals. Zero takes the corresponding Default above.
	HeartbeatInterval   time.Duration
	PreflightInterval   time.Duration
	MaintenanceInterval time.Duration
	ToolchainsInterval  time.Duration
	ClaimInterval       time.Duration
	SlotWait            time.Duration
	PreflightTimeout    time.Duration
	TeardownGrace       time.Duration
	UpdateInterval      time.Duration
	UpdateIdleWait      time.Duration

	// ShutdownGrace overrides agent.shutdown_grace. Zero uses the config
	// value, which itself defaults to 30 minutes because an iOS build is long.
	ShutdownGrace time.Duration

	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Agent is one machine's supervisor. Use [New], then [Agent.Run].
type Agent struct {
	cfg     *config.Config
	log     *slog.Logger
	version string
	now     func() time.Time
	opts    Options

	heartbeatEvery   time.Duration
	preflightEvery   time.Duration
	maintenanceEvery time.Duration
	claimEvery       time.Duration
	updateEvery      time.Duration
	updateIdleWait   time.Duration
	preflightTimeout time.Duration
	shutdownGrace    time.Duration
	teardownGrace    time.Duration

	// Built by start, torn down by stop, in reverse order.
	lock       *lockFile
	db         *store.DB
	src        *source.Manager
	caches     *cache.Local
	orch       *run.Orchestrator
	localBrk   *local.Broker
	remoteBrk  *remote.Broker
	brokers    []transport
	metrics    *metrics.Metrics
	metricsSrv *metrics.Server
	closers    []io.Closer

	runners []*runnerState
	slots   *slots

	// machineToken is §16's redactor content that no declared secret covers. It
	// is held so every per-run redactor can be given it; it is never logged and
	// never leaves this process.
	machineToken []byte

	// planSlots bounds the plans made at once; see [maxConcurrentPlans].
	planSlots chan struct{}

	// draining is set once shutdown begins or --drain-after fires. It is read
	// by the pool before every claim and by every heartbeat.
	draining atomic.Bool

	// paused stops the pool claiming without the one-way finality of
	// draining. Self-update and SIGUSR2 (A56) are its users, and the
	// difference matters:
	// draining is a decision to stop for good, and this is a pause that has to
	// end even when the thing it was waiting for failed (§20, selfupdate.go).
	paused atomic.Bool

	// wake is the doorbell: a run command arriving on a broker nudges the pool
	// rather than handing it the work, because the work is already in sqlite.
	wake chan struct{}

	ready     chan struct{}
	readyOnce sync.Once

	// live is the config a reload last applied, nil until one has (A54).
	// [Agent.current] reads it; cfg stays the config the agent started with,
	// which is the right answer for everything restart-only.
	live atomic.Pointer[config.Config]
	// reloadMu serialises reloads and status writes, and guards the fields
	// after it.
	reloadMu  sync.Mutex
	reloader  *config.Reloader
	reloads   int
	startedAt time.Time
	reloadErr string
	deferred  []string
	// binary is the file this agent runs, as its status reports it (A56).
	binary string
	// switchRequests counts SIGUSR2s and switchState is the last answer (A56).
	switchRequests int
	switchState    *SwitchState

	// switching is set while a move onto a new binary is pending, so a second
	// SIGUSR2, or the daily self-update, does not start another.
	switching atomic.Bool

	// toolchains is what ProbeToolchains last found (A59), every
	// toolchainsEvery or sooner when reprobe rings.
	toolchains      atomic.Pointer[system.Toolchains]
	toolchainsEvery time.Duration
	reprobe         chan struct{}
	// machine is what MeasureMachine last found (A63).
	machine atomic.Pointer[capacity.Machine]

	mu sync.Mutex
	// activeRepos maps a live run id to its repo key, so the cache pruner can
	// answer "is a live run reading this mirror" without a database round trip
	// on a path that already holds a lock (§14.4).
	activeRepos map[string]string
}

// New validates the options and builds an agent. It touches nothing on the
// machine: the lock, the database, and the socket all belong to [Agent.Run],
// because §6 fixes the order they are taken in and a constructor that took one
// of them early would make that order a coincidence.
func New(opts Options) (*Agent, error) {
	if opts.Config == nil {
		return nil, errors.New("agent: New: Config is required (RUNNER_SPEC §6 step 1)")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewExecutor == nil {
		opts.NewExecutor = DefaultExecutor
	}

	grace := opts.ShutdownGrace
	if grace <= 0 {
		grace = opts.Config.Agent.ShutdownGrace.Duration()
	}
	if grace <= 0 {
		grace = config.DefaultShutdownGrace
	}

	a := &Agent{
		cfg:              opts.Config,
		log:              opts.Logger,
		version:          orString(opts.Version, "dev"),
		now:              opts.Now,
		opts:             opts,
		heartbeatEvery:   orDuration(opts.HeartbeatInterval, DefaultHeartbeatInterval),
		preflightEvery:   orDuration(opts.PreflightInterval, DefaultPreflightInterval),
		maintenanceEvery: orDuration(opts.MaintenanceInterval, DefaultMaintenanceInterval),
		toolchainsEvery:  orDuration(opts.ToolchainsInterval, DefaultToolchainsInterval),
		reprobe:          make(chan struct{}, 1),
		claimEvery:       orDuration(opts.ClaimInterval, DefaultClaimInterval),
		updateEvery:      orDuration(opts.UpdateInterval, DefaultUpdateInterval),
		updateIdleWait:   orDuration(opts.UpdateIdleWait, DefaultUpdateIdleWait),
		preflightTimeout: orDuration(opts.PreflightTimeout, DefaultPreflightTimeout),
		teardownGrace:    orDuration(opts.TeardownGrace, DefaultTeardownGrace),
		shutdownGrace:    grace,
		wake:             make(chan struct{}, 1),
		planSlots:        make(chan struct{}, maxConcurrentPlans),
		ready:            make(chan struct{}),
		activeRepos:      map[string]string{},
	}
	return a, nil
}

// Ready is closed once §6 steps 2-8 have completed and the agent is serving.
// It exists for tests and for `--once`; an operator learns the same thing from
// the "agent ready" log line.
func (a *Agent) Ready() <-chan struct{} { return a.ready }

// Socket is the path the local broker listens on.
func (a *Agent) Socket() string { return a.cfg.Paths.Socket }

// MetricsAddr is the address the loopback metrics endpoint bound, or "" when
// metrics are disabled. It is only meaningful after [Agent.Ready] closes.
func (a *Agent) MetricsAddr() string {
	if a.metricsSrv == nil {
		return ""
	}
	return a.metricsSrv.Addr()
}

// Run performs RUNNER_SPEC §6 and blocks until ctx is cancelled — which for
// `latem agent` means SIGINT or SIGTERM — then drains and returns.
//
// It returns an error only for a startup failure. Once the agent is serving,
// the failures that matter are per-run and per-runner, and a supervisor that
// exited because Docker went away for a minute would be worse than one that
// marked a runner unavailable and kept the other two working.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.start(ctx); err != nil {
		a.stop(context.WithoutCancel(ctx))
		return err
	}
	defer a.stop(context.WithoutCancel(ctx))

	// Work runs under a context the signal cannot reach. Cancelling the root
	// context must stop the agent *accepting* runs; it must not abort the
	// build that is forty minutes into an archive, which is the whole point of
	// shutdown_grace.
	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()

	var wg sync.WaitGroup
	a.spawn(&wg, func() { a.consumeCommands(workCtx) })
	a.spawn(&wg, func() { a.heartbeatLoop(workCtx) })
	a.spawn(&wg, func() { a.preflightLoop(workCtx) })
	a.spawn(&wg, func() { a.maintenanceLoop(workCtx) })
	a.spawn(&wg, func() { a.toolchainsLoop(workCtx) })
	a.spawn(&wg, func() { a.updateLoop(workCtx) })

	pool := a.startPool(workCtx)
	a.startReloads()
	defer a.removeReloadStatus()
	a.markReady()
	a.log.Info("agent ready",
		"machine", a.cfg.Machine.Name,
		"socket", a.cfg.Paths.Socket,
		"runners", len(a.runners),
		"metrics", a.MetricsAddr())

	if a.opts.Once {
		a.once(workCtx, pool)
	} else {
		a.block(ctx, pool)
	}

	// Graceful shutdown (§6). Stop accepting, keep beating with Draining set,
	// let the in-flight work finish, then signal it.
	a.beginDrain()
	pool.stopAccepting()
	a.awaitDrain(pool)

	stopWork()
	pool.wait()
	wg.Wait()
	return nil
}

// block waits for the signal context, for --drain-after, or for the pool to
// finish everything it will ever do.
func (a *Agent) block(ctx context.Context, pool *pool) {
	var drain <-chan time.Time
	if a.opts.DrainAfter > 0 {
		t := time.NewTimer(a.opts.DrainAfter)
		defer t.Stop()
		drain = t.C
	}
	select {
	case <-ctx.Done():
		a.log.Info("signal received, draining", "grace", a.shutdownGrace)
	case <-drain:
		a.log.Info("drain-after elapsed, draining", "after", a.opts.DrainAfter)
	case <-pool.done():
	}
}

// once executes a single claimable run and returns (`latem agent --once`).
//
// It exits rather than idling on an empty queue. A debugging aid that blocks
// forever when there is nothing to debug is one people stop reaching for.
func (a *Agent) once(ctx context.Context, pool *pool) {
	r, err := a.claimOne(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.log.Info("--once: nothing claimable in the queue")
		return
	case err != nil:
		a.log.Error("--once: claiming", "err", err)
		return
	}
	pool.execute(ctx, r)
}

// awaitDrain waits out shutdown_grace, then cancels the in-flight runs and
// gives their teardown a bounded moment to remove containers and VMs.
//
// The second wait is not politeness. §6 says containers and VMs are torn down
// on every path "even on the kill path", and internal/run registers that
// teardown against a context cancellation cannot reach — but the process still
// has to be alive when it runs. Returning the instant the grace expires would
// make the agent the thing that leaks the 90 GB VM.
func (a *Agent) awaitDrain(pool *pool) {
	if pool.idle() {
		return
	}

	a.log.Info("waiting for in-flight runs", "runs", pool.activeIDs(), "grace", a.shutdownGrace)
	grace := time.NewTimer(a.shutdownGrace)
	defer grace.Stop()

	select {
	case <-pool.done():
		a.log.Info("in-flight runs finished")
		return
	case <-grace.C:
	}

	a.log.Warn("shutdown grace expired, signalling in-flight runs", "runs", pool.activeIDs())
	pool.cancelAll("agent shutting down")

	teardown := time.NewTimer(a.teardownGrace)
	defer teardown.Stop()
	select {
	case <-pool.done():
		a.log.Info("in-flight runs torn down")
	case <-teardown.C:
		a.log.Error("runs did not tear down in time; reconciliation will clean up at the next start",
			"runs", pool.activeIDs())
	}
}

// beginDrain marks the machine draining and publishes one heartbeat saying so,
// so a control plane sees the transition immediately rather than up to fifteen
// seconds later.
func (a *Agent) beginDrain() {
	if a.draining.Swap(true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.publishHealth(ctx)
}

// Draining reports whether the agent has stopped accepting new runs.
func (a *Agent) Draining() bool { return a.draining.Load() }

func (a *Agent) markReady() { a.readyOnce.Do(func() { close(a.ready) }) }

func (a *Agent) spawn(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}

// transport is one broker and the name it is known by, which is also the key
// its state is stored under (store.BrokerLocal, store.BrokerRemote). Keeping
// the two the same means a log line about a transport and a row about a
// transport name it identically, and there is no second vocabulary to learn.
type transport struct {
	name string
	b    broker.Broker
}

// brokerList is what the orchestrator fans out to.
func (a *Agent) brokerList() []broker.Broker {
	out := make([]broker.Broker, 0, len(a.brokers))
	for _, t := range a.brokers {
		out = append(out, t.b)
	}
	return out
}

// consumeCommands is §6 step 7's reading half: every configured transport, read
// concurrently, handled identically.
//
// A run command is a doorbell and nothing more. Both transports have already
// made the run durable in sqlite before delivering it — internal/broker/local
// on the socket handler, internal/broker/remote in [handleCommand] — so the
// only thing left to do is wake a worker. Acting on the request carried in the
// command instead would introduce a second path into execution, one that works
// only when the agent happens to be up, and the two would drift.
//
// One goroutine per transport rather than a merged channel. There is nothing to
// order between them: a run is a wake-up and a cancel addresses one run id, so
// interleaving is not a hazard, and a merger would only add a place for one
// silent transport to hold up another.
//
// The remote transport replays a backlog after a reconnect (see
// internal/broker/remote's package comment), which is why both handlers have to
// be idempotent by run id — and both are. Ringing the doorbell twice is one
// doorbell, and §7.1 defines cancel as a no-op for an unknown, unclaimed, or
// terminal id.
func (a *Agent) consumeCommands(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range a.brokers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.consumeFrom(ctx, t)
		}()
	}
	wg.Wait()
}

// consumeFrom drains one transport's command stream until it closes.
//
// A transport that will not yield commands is logged and abandoned, and nothing
// else changes: the other transports keep being read, the runner pool keeps
// claiming from the queue, and the machine keeps building. That is the same
// promise §7.4 makes about a revoked token, applied one layer up.
func (a *Agent) consumeFrom(ctx context.Context, t transport) {
	cmds, err := t.b.Commands(ctx)
	if err != nil {
		a.log.Error("broker command stream", "broker", t.name, "err", err)
		return
	}
	for cmd := range cmds {
		switch cmd.Kind {
		case protocol.CommandRun:
			a.log.Info("run queued",
				"run_id", cmd.Run.ID, "repo", cmd.Run.Repo, "job", cmd.Run.Job, "broker", t.name)
			a.ring()
		case protocol.CommandCancel:
			a.cancel(ctx, *cmd.Cancel)
		case protocol.CommandPlan:
			go a.plan(ctx, t, *cmd.Plan)
		}
	}
}

func (a *Agent) cancel(ctx context.Context, req protocol.CancelRequest) {
	if err := a.orch.Cancel(ctx, req); err != nil {
		a.log.Error("cancel", "run_id", req.ID, "err", err)
		return
	}
	a.log.Info("cancel accepted", "run_id", req.ID, "actor", req.Actor)
}

// ring wakes one waiting worker. It never blocks: the buffer is one, and a
// second notification arriving before the first was taken is the same
// notification.
func (a *Agent) ring() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// noteActive records the repo a live run is reading, for [Agent.mirrorBusy].
func (a *Agent) noteActive(runID, repo string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeRepos[runID] = repo
}

func (a *Agent) forgetActive(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.activeRepos, runID)
}

// mirrorBusy reports whether a live run is reading this mirror.
//
// Mirrors have gc.auto disabled (§12) precisely so nothing collects the commit
// a build is checking out; running an explicit gc under one would undo that,
// with a symptom — a build failing on a missing object after a force-push —
// that points nowhere near the daily ticker.
func (a *Agent) mirrorBusy(mirror string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, repo := range a.activeRepos {
		if a.cfg.Paths.MirrorDir(repo) == mirror {
			return true
		}
	}
	return false
}

// isLive is the predicate the cache pruner, the log pruner, and every
// Executor.Reap consult. It is the orchestrator's view rather than the store's,
// because a run that this process is executing right now is live whatever a
// row says, and a sweep that deleted its workspace would break a build that is
// working perfectly.
func (a *Agent) isLive(runID string) bool {
	return a.orch != nil && a.orch.IsLive(runID)
}

// health assembles the §7.2 MachineHealth published every fifteen seconds.
func (a *Agent) health(ctx context.Context) protocol.MachineHealth {
	h := protocol.MachineHealth{
		MachineID: a.cfg.Machine.ID,
		AgentVer:  a.version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Draining:  a.draining.Load(),
		Runners:   make([]protocol.RunnerInfo, 0, len(a.runners)),
	}
	for _, r := range a.runners {
		h.Runners = append(h.Runners, r.info())
	}
	// A61: what it has for builds, once the background probe has landed.
	// The remote broker sends it only to a control plane that says 2.2.
	if tc := a.toolchains.Load(); tc != nil {
		h.Toolchains = toolchainsAdvert(*tc)
	}
	// A63: its size and each runner's recommended concurrency, worked out
	// against the config as it is now. Sent only to a control plane that
	// says 2.3.
	if m := a.machine.Load(); m != nil && m.CPUs > 0 && m.MemoryGB > 0 {
		h.Capacity = capacityAdvert(*m, a.current().Runners)
	}
	// A52: the repositories this machine builds, by key and by owner/name,
	// which is how an event on GitHub finds the machines that can plan it.
	repos := a.current().Repos
	for _, key := range slices.Sorted(maps.Keys(repos)) {
		info := protocol.RepoInfo{Key: key}
		if slug, ok := repos[key].GitHubRepository(); ok {
			info.GitHub = slug
		}
		h.Repos = append(h.Repos, info)
	}
	if a.caches != nil {
		if free, err := a.caches.FreeGB(ctx); err == nil {
			h.DiskFreeGB = free
		}
	}
	return h
}

// publishHealth sends one beat to every broker and updates the local gauges.
func (a *Agent) publishHealth(ctx context.Context) {
	h := a.health(ctx)
	for _, t := range a.brokers {
		if err := t.b.Heartbeat(ctx, h); err != nil {
			a.log.Warn("heartbeat", "broker", t.name, "err", err)
		}
	}
	a.recordHealth(h)
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.heartbeatEvery)
	defer t.Stop()
	a.publishHealth(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Heartbeats continue while draining, with Draining set: a machine
			// that stops beating and a machine that is shutting down have to
			// look different to whoever is watching (§20).
			hctx, cancel := context.WithTimeout(ctx, a.heartbeatEvery)
			a.publishHealth(hctx)
			cancel()
		}
	}
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// errNoRunners is what an agent with no usable runner reports at startup.
var errNoRunners = errors.New("agent: no runners configured; add a [[runner]] block to config.toml (RUNNER_SPEC §8)")

func wrapStartup(step string, err error) error {
	return fmt.Errorf("agent: %s: %w", step, err)
}
