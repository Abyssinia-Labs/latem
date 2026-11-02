package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/broker/remote"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/executor/docker"
	"github.com/Abyssinia-Labs/latem/internal/executor/native"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/metrics"
	"github.com/Abyssinia-Labs/latem/internal/run"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
)

// DefaultExecutor builds the executor a runner's config names (§13).
//
// Tart is absent on purpose rather than stubbed. WS-13 owns
// internal/executor/tart and it does not exist yet; returning an error here
// registers the runner as permanently unavailable with that sentence as its
// reason, which is what an operator needs to see. A stub that accepted work
// and then failed every build would be worse than a runner that says it is not
// there.
func DefaultExecutor(r *config.Runner, paths config.Paths) (executor.Executor, error) {
	switch r.Executor {
	case config.ExecutorDocker:
		opts := docker.Options{}
		if r.Docker != nil {
			opts.DefaultImage = r.Docker.DefaultImage
			opts.GPU = r.Docker.GPU
			opts.RequirePinnedImages = r.Docker.RequirePinnedImages
		}
		return docker.New(opts)

	case config.ExecutorNative:
		// The $LATEM_ENV directory is deliberately not the workspace: the
		// workspace is a git clone and a file appearing in it would show up in
		// `git status`. It is kept under the config directory rather than
		// $TMPDIR so --config-dir relocates it with everything else, and so a
		// second agent's reap cannot find this one's live runs (§8).
		dir := paths.RunnerCacheDir(r.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("agent: runner %q: %w", r.Name, err)
		}
		return native.New(native.Options{
			EnvFileDir: dir,
			// An iOS runner cannot sign code in a daemon context, and
			// discovering that at the codesign step produces a failure whose
			// log mentions nothing useful (§23).
			RequireGUISession: hasLabel(r.Labels, "ios"),
			Env:               r.NativeEnv(),
			PathPrepend:       r.NativePathPrepend(),
		})

	case config.ExecutorTart:
		return nil, errors.New("the tart executor is not implemented yet (WS-13); " +
			`use executor = "native" for macOS work until it lands`)

	default:
		return nil, fmt.Errorf("unknown executor %q", r.Executor)
	}
}

// stepPath is the PATH a native step starts from: the agent's own, behind
// every native runner's path_prepend (A60). The probe is one for the machine,
// so where two native runners prepend differently it finds a tool on either.
func stepPath(cfg *config.Config, agentPath string) string {
	var prepend []string
	for _, r := range cfg.Runners {
		for _, dir := range r.NativePathPrepend() {
			if !slices.Contains(prepend, dir) {
				prepend = append(prepend, dir)
			}
		}
	}
	return ghcmd.PrependPath(prepend, agentPath)
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// start performs RUNNER_SPEC §6 steps 2 through 8.
//
// Every failure here is fatal and every one names what to do about it, because
// this runs before there is a log pipeline, a run, or anything else to
// diagnose from. The caller unwinds with [Agent.stop], which is safe against a
// partially built agent — which is why each field is assigned as soon as it
// exists rather than at the end.
//
// One deviation from §6's numbering is deliberate and worth stating. Both
// brokers are opened before the orchestrator rather than at step 7, because
// internal/run takes its brokers at construction and things only a broker does
// are not optional: [local.Broker.Report] marks broker_state.reported_state,
// which is what store.Unreported keys on — an agent that never marked anything
// reported would replay its whole history on every reconnect — and each
// transport's Logs is what persists its own last_acked_seq, the resume point of
// finding F6. Nothing acts on an inbound command before the pool starts, so
// binding the socket earlier changes where a run is recorded, not when it
// executes.
func (a *Agent) start(ctx context.Context) error {
	if len(a.cfg.Runners) == 0 {
		return errNoRunners
	}

	// Step 2 — the flock. Before sqlite, so two agents cannot race a schema
	// migration, and before the socket, so the second agent fails with a pid
	// rather than with "address already in use".
	lock, err := acquireLock(a.cfg.Paths.Lock)
	if err != nil {
		return err
	}
	a.lock = lock

	// Step 3 — sqlite and migrations. The redactor is required rather than
	// optional: inputs_json and cancel_reason are scrubbed on the way in
	// (INV-4), and a nil one would satisfy the interface silently. It is built
	// here rather than inline so the machine token can go into it before the
	// first row is written.
	red := logstream.NewRedactor()
	a.loadMachineToken(red)

	db, err := store.Open(ctx, a.cfg.Paths.DB, red)
	if err != nil {
		return wrapStartup("opening the state database", err)
	}
	a.db = db
	if err := db.Migrate(ctx); err != nil {
		return wrapStartup("migrating the state database", err)
	}

	if err := a.buildRunners(); err != nil {
		return err
	}
	if err := a.buildDependencies(); err != nil {
		return err
	}
	if err := a.startLocalBroker(); err != nil {
		return err
	}
	if err := a.buildOrchestrator(); err != nil {
		return err
	}

	// Step 4 — preflight, before anything is claimable. A runner that fails
	// starts unavailable rather than accepting the first run and failing it.
	a.preflightAll(ctx)

	// Step 5 — reconciliation. Not optional.
	if _, err := a.Reconcile(ctx); err != nil {
		// A reconciliation that partly failed still leaves the machine better
		// off than no agent at all, and refusing to start would mean one
		// undeletable container costs an operator their whole build machine.
		// It is logged loudly and the daily prune tries again.
		a.log.Error("reconciliation did not complete; some resources may still be orphaned", "err", err)
	}

	a.startMetrics()
	return nil
}

// buildRunners turns each [[runner]] block into a live slot.
//
// A runner whose executor cannot be constructed is kept rather than dropped:
// it appears in every heartbeat as unavailable with the reason, and it is
// withheld from the orchestrator so gate 13 cannot select it. Dropping it
// would make a machine with a broken Docker socket look like a machine with no
// docker runner configured, which sends the operator to the wrong file.
func (a *Agent) buildRunners() error {
	a.runners = make([]*runnerState, 0, len(a.cfg.Runners))
	usable := 0
	for _, rc := range a.cfg.Runners {
		ex, err := a.opts.NewExecutor(rc, a.cfg.Paths)
		if err != nil {
			a.log.Error("runner unavailable", "runner", rc.Name, "executor", rc.Executor, "err", err)
			a.runners = append(a.runners, newRunnerState(rc, nil, err))
			continue
		}
		a.runners = append(a.runners, newRunnerState(rc, ex, nil))
		usable++
	}
	if usable == 0 {
		return errors.New("agent: no runner has a working executor; `latem doctor` names the fix")
	}
	a.slots = newSlots(a.runners, orDuration(a.opts.SlotWait, DefaultSlotWait))
	return nil
}

// buildDependencies constructs everything the orchestrator consumes.
func (a *Agent) buildDependencies() error {
	src, err := source.New(source.Config{
		MirrorDir: a.cfg.Paths.Mirrors,
		WorkDir:   a.cfg.Paths.Work,
		Logger:    a.log,
	})
	if err != nil {
		return wrapStartup("source acquisition", err)
	}
	a.src = src

	runners := make(map[string]cache.RunnerConfig, len(a.runners))
	needDocker := false
	for _, r := range a.runners {
		runners[r.name] = cache.RunnerConfig{Executor: r.executor, MinFreeGB: r.cfg.MinFreeGB()}
		if r.cfg.Executor == config.ExecutorDocker {
			needDocker = true
		}
	}

	opts := cache.Options{
		Root:       a.cfg.Paths.Dir,
		WorkDir:    a.cfg.Paths.Work,
		MirrorDir:  a.cfg.Paths.Mirrors,
		Runners:    runners,
		Live:       a.isLive,
		MirrorBusy: a.mirrorBusy,
		Now:        a.now,
	}
	if needDocker {
		// An unreachable daemon is the docker runner's preflight problem, not
		// a reason the whole cache subsystem should refuse to start: every
		// other runner's host-directory caches still work.
		if cli, cerr := dockerclient.NewClientWithOpts(
			dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation()); cerr == nil {
			opts.Docker = cache.NewDockerAPI(cli)
			a.closers = append(a.closers, cli)
		} else {
			a.log.Warn("docker daemon unreachable; volume-backed caches are disabled", "err", cerr)
		}
	}

	caches, err := cache.New(opts)
	if err != nil {
		return wrapStartup("cache manager", err)
	}
	a.caches = caches
	return nil
}

// buildOrchestrator wires internal/run.
func (a *Agent) buildOrchestrator() error {
	provider := a.opts.Secrets
	if provider == nil {
		chain, err := secrets.New(secrets.Options{
			FilePath: a.cfg.Paths.SecretsEnv,
			Commands: a.cfg.Secrets.Commands,
		})
		if err != nil {
			return wrapStartup("secrets", err)
		}
		provider = chain
	}

	runners := make([]run.Runner, 0, len(a.runners))
	for _, r := range a.runners {
		if !r.usable() {
			continue
		}
		runners = append(runners, run.NewRunner(r.cfg, &gatedExecutor{
			inner: r.ex,
			state: r,
			slots: a.slots,
		}))
	}

	// An empty broker list is legal here and would still build every run
	// correctly; see the ordering note on [Agent.start] for why it is not what
	// happens. Every configured transport is passed, and internal/run fans
	// reports and log frames out to all of them.
	orch, err := run.New(run.Options{
		Config:       a.cfg,
		Store:        a.db,
		Brokers:      a.brokerList(),
		AlwaysRedact: a.alwaysRedact(),
		Source:       run.SourceManager(a.src),
		Secrets:      provider,
		Caches:       a.caches,
		Artifacts:    artifact.NewCollector(a.cfg.Paths.Artifacts),
		Runners:      runners,
		Now:          a.now,
		Toolchains:   a.toolchains.Load,
	})
	if err != nil {
		return wrapStartup("orchestrator", err)
	}
	a.orch = orch
	return nil
}

// startLocalBroker binds the Unix socket (§7.3).
func (a *Agent) startLocalBroker() error {
	if !a.cfg.Broker.Local.Enabled {
		a.log.Warn("broker.local is disabled; `latem run` cannot reach this agent")
		return nil
	}
	brk, err := local.Open(local.Options{
		Socket:   a.cfg.Paths.Socket,
		Store:    a.db,
		QueueTTL: a.queueTTL(),
		Logger:   a.log,
	})
	if err != nil {
		return wrapStartup("local broker", err)
	}
	a.localBrk = brk
	a.brokers = append(a.brokers, transport{name: store.BrokerLocal, b: brk})
	return a.startRemoteBroker()
}

// startRemoteBroker connects to the control plane (§7.4), when one is
// configured.
//
// A failure here is not a startup failure, and that is the point of the whole
// transport. [remote.Open] does not touch the network — it starts a reconnect
// loop and returns — so an unreachable control plane costs this agent nothing,
// and a revoked token stops the loop while the local broker keeps serving. The
// only error it can return is a configuration one, and that is fatal for the
// same reason every other §8 mistake is: an agent that starts on a config it
// could not make sense of fails later, inside somebody's build.
//
// [Agent.brokers] is the fan-out seam. Heartbeats, commands, state transitions
// and log frames all go to every entry: [Agent.publishHealth],
// [Agent.consumeCommands], and internal/run through [Agent.brokerList].
//
// That fan-out was impossible until amendment A28. §22 gave `reported_state`
// and `last_acked_seq` one column each for the whole machine, and the local
// broker wrote both the moment a transition or a batch was persisted, so a
// fanned-out sink would have advanced the remote transport's resume point to
// "everything this machine has emitted" — silently losing exactly the frames
// covering an outage. The `broker_state` table keys both by transport, and each
// transport now writes and reads only its own row.
func (a *Agent) startRemoteBroker() error {
	if !a.cfg.Broker.Remote.Enabled {
		return nil
	}
	brk, err := remote.Open(remote.Options{
		URL:       a.cfg.Broker.Remote.URL,
		TokenFile: a.cfg.Paths.TokenFile,
		MachineID: a.cfg.Machine.ID,
		Store:     a.db,
		QueueTTL:  a.queueTTL(),
		LogMode:   remote.LogMode(a.cfg.Agent.RemoteLogs),
		Logger:    a.log,
	})
	if err != nil {
		return wrapStartup("remote broker", err)
	}
	a.remoteBrk = brk
	a.brokers = append(a.brokers, transport{name: store.BrokerRemote, b: brk})
	a.log.Info("remote broker configured", "url", a.cfg.Broker.Remote.URL, "remote_logs", a.cfg.Agent.RemoteLogs)
	return nil
}

// loadMachineToken registers the machine token with red and keeps a copy for
// every per-run redactor (RUNNER_SPEC §16, INV-4).
//
// §16's "redactor contents" lists the machine token first and nothing in the
// tree registered it: internal/secrets registers what a job *declares*, and the
// token is not a declared secret — it is the credential that authenticates this
// machine to the control plane, held in its own file precisely so it is never
// an environment variable anybody could declare. A build that printed it — a
// script that cats ~/.latem/agent.key, a support bundle, a `set -x` over the
// config directory — put it in the log file and shipped it to the control plane
// with INV-4's tests passing.
//
// A missing or unreadable file is not a startup failure. The token is optional
// on a machine with no control plane, and when there is one [remote.Broker]
// says so far more usefully than this could: it names the file, the fix, and the
// fact that local operation is unaffected.
//
// It is read once, here. Rotating the token with `latem register` takes effect
// on the transport at the next reconnect, because that path re-reads the file
// per dial; for redaction it takes effect at the next restart. The old value
// stays registered either way, which is the safe direction — a rotated-out token
// is still a secret, and the window in which the new one is unregistered is the
// window in which nothing on this machine has printed it yet.
func (a *Agent) loadMachineToken(red logstream.Registrar) {
	path := a.cfg.Paths.TokenFile
	token, err := remote.ReadToken(path)
	if err != nil {
		if a.cfg.Broker.Remote.Enabled {
			a.log.Warn("the machine token could not be read, so it cannot be kept out of build logs",
				"token_file", path, "err", err)
		} else {
			a.log.Debug("no machine token to register with the redactor", "token_file", path, "err", err)
		}
		return
	}

	value := []byte(token)
	if logstream.TooShort(value) {
		// Named, never printed — the same rule internal/secrets follows. Short
		// values are not redacted at all (§16), so an operator with a two-letter
		// token has one that will appear in logs verbatim and nothing else would
		// ever tell them.
		a.log.Warn("the machine token is too short to redact and will appear in build logs verbatim",
			"token_file", path, "min_length", logstream.MinRedactableLength)
		return
	}

	red.Register(value)
	a.machineToken = value
}

// alwaysRedact is what every run's redactor is seeded with (§16).
func (a *Agent) alwaysRedact() [][]byte {
	if len(a.machineToken) == 0 {
		return nil
	}
	return [][]byte{a.machineToken}
}

func (a *Agent) queueTTL() time.Duration {
	if d := a.cfg.Agent.QueueTTL.Duration(); d > 0 {
		return d
	}
	return config.DefaultQueueTTL
}

// startMetrics binds the loopback endpoint (§20).
//
// The address was already checked for loopback by the config loader, which is
// INV-1 enforced where it cannot be forgotten; [metrics.Serve] checks again
// because the listener is the last place the rule can still be broken.
func (a *Agent) startMetrics() {
	a.metrics = metrics.New(a.version)
	if !a.cfg.MetricsEnabled() {
		return
	}
	srv, err := metrics.Serve(a.cfg.Agent.MetricsAddr, a.metrics.Handler())
	if err != nil {
		// Metrics are observability, not operation. An agent that refused to
		// build anything because port 9838 was taken would be trading the
		// product for a graph.
		a.log.Error("metrics endpoint not started", "addr", a.cfg.Agent.MetricsAddr, "err", err)
		return
	}
	a.metricsSrv = srv
	a.log.Info("metrics listening", "addr", srv.Addr())
}

// stop tears the agent down in reverse order, releasing the lock last (§6).
//
// Every step runs even if an earlier one failed. Shutdown is the path that
// leaks a 90 GB VM if it gives up early, and the errors here are almost always
// "already closed", which is not a reason to skip the next resource.
func (a *Agent) stop(ctx context.Context) {
	if a.metricsSrv != nil {
		_ = a.metricsSrv.Close(ctx)
		a.metricsSrv = nil
	}
	if a.remoteBrk != nil {
		// Before the local broker, so that the last thing a control plane hears
		// is a clean close rather than a connection that stopped answering.
		if err := a.remoteBrk.Close(); err != nil {
			a.log.Warn("closing the remote broker", "err", err)
		}
		a.remoteBrk = nil
	}
	if a.localBrk != nil {
		if err := a.localBrk.Close(); err != nil {
			a.log.Warn("closing the local broker", "err", err)
		}
		a.localBrk = nil
	}
	a.brokers = nil
	if a.orch != nil {
		_ = a.orch.Close()
	}
	for _, r := range a.runners {
		if r.closer != nil {
			_ = r.closer.Close()
		}
	}
	for _, c := range a.closers {
		_ = c.Close()
	}
	a.closers = nil
	if a.db != nil {
		if err := a.db.Close(); err != nil {
			a.log.Warn("closing the state database", "err", err)
		}
		a.db = nil
	}
	// Last, per §6. Until this returns, another agent must not be able to
	// start against this directory, because the containers this one was
	// removing may still be going away.
	if a.lock != nil {
		if err := a.lock.Close(); err != nil {
			a.log.Warn("releasing the agent lock", "err", err)
		}
		a.lock = nil
	}
	a.log.Info("agent stopped")
}

var _ io.Closer = (*lockFile)(nil)
