package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// storeTimeout bounds one state transition. Transitions run on a context
// detached from the run's, so they need their own deadline.
const storeTimeout = 30 * time.Second

// execution is the state of one run in flight. Everything mutable about a run
// lives here rather than on the [Orchestrator], which is what lets one
// orchestrator serve every runner slot on the machine at once.
type execution struct {
	o   *Orchestrator
	req protocol.RunRequest
	run store.Run
	h   *handle

	started time.Time

	red     logstream.Redactor
	emitter *logstream.Emitter
	// deferred holds notes produced before the emitter existed.
	//
	// The broker sinks themselves are not held here. Each one is owned by the
	// batching wrapper the emitter closes, and since A28 nothing outside the
	// transport needs to read a sink's resume point — the transport persists its
	// own, under its own key.
	deferred []string

	ws       *source.Workspace
	resolved *registry.Resolved
	// refNormalized and sha are what INV-5 decided at gates 7 and 9. They ride
	// on the next transition rather than one of their own (amendment A21).
	refNormalized string
	sha           string
	runner        *Runner
	sec           *secrets.Set
	env           executor.Env
	// outputs is every completed step's $GITHUB_OUTPUT, by step id, for later
	// steps' late bindings. Populated once that channel is wired (A42 phase
	// 2); until then the compiler refuses steps.* and this stays empty.
	outputs map[string]map[string]string

	teardowns []teardownStep
}

// teardownStep is one resource's release, registered the moment the resource
// exists. RUNNER_SPEC §11: "Teardown is defer-registered at resource creation,
// so no path leaks a container or VM."
type teardownStep struct {
	what string
	fn   func(context.Context) error
}

// Execute drives a claimed run to a terminal state.
//
// It returns an error only for conditions that stopped the run being executed
// at all — a run that is not claimed, a run already executing here, a store
// that refused the first transition. Everything a build can do to itself,
// including failing, timing out and being cancelled, comes back as a [Result].
// A caller that treats a failed build as an error ends up retrying it.
func (o *Orchestrator) Execute(ctx context.Context, r store.Run) (Result, error) {
	if err := protocol.ValidateID(r.ID); err != nil {
		return Result{}, fmt.Errorf("run: execute: %w", err)
	}
	if r.State != protocol.StateClaimed {
		return Result{}, fmt.Errorf("%w: %s is %s", ErrNotClaimed, r.ID, r.State)
	}

	runCtx, h, err := o.begin(ctx, r.ID)
	if err != nil {
		return Result{}, err
	}
	defer o.end(h)
	defer o.reporter.forget(r.ID)

	x := &execution{o: o, req: requestOf(r), run: r, h: h, started: o.now()}
	return x.drive(runCtx), nil
}

// drive is the §11 state machine for one run.
func (x *execution) drive(ctx context.Context) Result {
	if err := x.openLogs(ctx); err != nil {
		// Without a log pipeline there is nowhere to narrate a failure to, so
		// this one is reported through the state machine alone.
		return x.terminate(ctx, fail(err, protocol.ErrExecutor))
	}
	defer x.closeLogs(ctx)
	defer x.teardown(ctx)

	// claimed → preparing.
	if err := x.to(ctx, protocol.StatePreparing, protocol.Update{StartedAt: &x.started}); err != nil {
		return x.raced(err)
	}

	// Gates 6-11: fetch, normalize, policy, resolve, clone, and read the
	// registry off the workspace at the pinned commit (INV-5).
	reg, dec, err := x.acquire(ctx)
	switch {
	case err != nil:
		return x.finish(ctx, fail(err, protocol.ErrSource))
	case dec.State == protocol.StateAwaitingApproval:
		return x.awaitApproval(ctx, dec)
	case !dec.OK():
		return x.finish(ctx, fromDecision(dec))
	}

	// Gates 12-15: job lookup, label match, typed inputs, declared secrets.
	runner := x.o.selectRunner(reg, x.req.Job)
	var labels []string
	if runner != nil {
		labels = runner.Labels
	}
	resolved, dec := reg.Admit(x.req, labels)
	if !dec.OK() {
		return x.finish(ctx, fromDecision(dec))
	}
	x.resolved, x.runner = resolved, runner
	x.note("job %q on runner %q at %s (budget %s, %d step(s))",
		resolved.JobName, runner.Name, shortSHA(x.ws.SHA), resolved.Timeout, len(resolved.Steps))

	// The job wall clock is §9's, "enforced by the agent, not the executor",
	// and it covers the whole job — which includes the rest of preparing. §11
	// draws preparing → timed_out for exactly this reason: a checkout or a
	// cache lock that never returns is as much a job overrun as a hung step.
	deadline := x.started.Add(resolved.Timeout)
	jobCtx, cancelJob := context.WithDeadline(ctx, deadline)
	defer cancelJob()

	// Gate 15, for real: resolve the values behind the declared names. §15
	// makes this an admission answer rather than "a runtime surprise 40
	// minutes in", and ResolveAll registers every value with the redactor
	// before it returns one (INV-4).
	if out, ok := x.resolveSecrets(jobCtx); !ok {
		return x.finish(ctx, out)
	}

	// Gate 16: concurrency group. Queued, not rejected.
	if group := resolved.ConcurrencyGroup; group != "" {
		if !x.o.groups.acquire(group, x.run.ID) {
			return x.requeue(ctx, group)
		}
		x.onTeardown("concurrency group "+group, func(context.Context) error {
			x.o.groups.release(group, x.run.ID)
			return nil
		})
	}

	// Gate 17: this runner's own disk floor, which gate 5 could not know.
	if err := x.checkDisk(jobCtx); err != nil {
		return x.finish(ctx, fail(err, protocol.ErrDisk))
	}

	// Gate 18: cache volumes and their locks.
	mounts, err := x.acquireCaches(jobCtx)
	if err != nil {
		return x.finish(ctx, fail(err, protocol.ErrCache))
	}

	if err := x.prepareEnv(jobCtx, mounts); err != nil {
		return x.finish(ctx, fail(err, protocol.ErrExecutor))
	}

	// Gate 19: execute.
	if err := x.to(jobCtx, protocol.StateRunning, protocol.Update{Runner: runner.Name}); err != nil {
		return x.raced(err)
	}
	return x.finish(ctx, x.runSteps(jobCtx, resolved.Timeout))
}

// acquire runs gates 6 through 11.
//
// Gate 11 is the one the rest of the system is built around. The registry is
// read from the workspace — a real checkout at the SHA gate 9 resolved — and
// never from anything a broker said. A control plane supplies the job's name;
// this is where the name becomes something executable, or does not.
func (x *execution) acquire(ctx context.Context) (*registry.Registry, registry.Decision, error) {
	policy, repoCfg, ok := x.o.repoPolicy(x.req.Repo)
	if !ok {
		// Gate 1 again. The allowlist can have changed between admission and
		// the claim, and building a repo that is no longer configured because
		// a row said so would make the config advisory rather than binding.
		return nil, registry.Decision{
			State: protocol.StateRejected,
			Code:  protocol.ErrUnknownRepo,
			Msg:   fmt.Sprintf("repo %q is not configured on this machine", x.req.Repo),
		}, nil
	}
	if x.o.src == nil {
		return nil, registry.Decision{}, errors.New("run: no source manager configured")
	}

	// Gate 6 — mirror fetch, under internal/source's per-repo mutex.
	x.note("fetching %s", x.req.Repo)
	mirror, err := x.o.src.Fetch(ctx, source.Repo{Key: x.req.Repo, URL: repoCfg.URL})
	if err != nil {
		return nil, registry.Decision{}, err
	}

	// Gate 7 — normalization. Exactly one of refs/heads/<ref> and
	// refs/tags/<ref> must exist; both is E_AMBIGUOUS_REF and never a guess.
	normalized, err := registry.NormalizeRef(ctx, mirror, x.req.Ref)
	if err != nil {
		return nil, registry.Decision{}, err
	}

	// Gate 9 — resolve, BEFORE gate 8 decides (amendment A27). §10 used to
	// order these the other way, and that ordering is what made INV-5's
	// approval unimplementable: gate 8 parked a run without knowing which
	// commit it was parking, so `latem approve` had nothing to bind to and the
	// only approval it could express was "this ref, whenever it eventually
	// builds" — a force-push away from meaningless.
	//
	// Resolving first is safe, and cheap. It is a `git rev-parse` against a
	// mirror that gate 6 has already fetched: a local read, no execution, no
	// clone, and nothing from the commit is trusted or even looked at. What it
	// buys is that every parked run records exactly the commit an operator is
	// being asked to approve.
	sha, err := mirror.Resolve(ctx, normalized)
	if err != nil {
		return nil, registry.Decision{}, err
	}

	// Record what INV-5 decided, in the columns §22 keeps for it (amendment
	// A21). The pair is settled here, at gates 7 and 9, long after store.Create
	// — which is why Update had to grow the fields — and it is the answer to
	// the only question anyone asks about a surprising build: requested as
	// "main", built refs/heads/main at 3bf1261.
	//
	// They are set before gate 8 rather than after it because a parked run
	// needs them most: they ride on whatever transition comes next, including
	// the one into awaiting_approval.
	x.refNormalized, x.sha = normalized, sha

	// Gate 8 — authoritative ref policy, against the name that actually exists
	// rather than the ones that might have, and against the commit that name
	// resolves to right now (INV-5).
	//
	// The approval is classified here rather than in internal/registry: which
	// commit was approved is a fact this row carries, and gate 8 decides policy
	// from names. A ref that has moved since it was approved comes back as a
	// fresh park, not as a yes.
	approval := registry.ApprovalFor(x.run.ApprovedSHA, sha)
	if d := registry.AdmitRef(x.run.ID, x.req.Repo, normalized, policy, approval); !d.OK() {
		return nil, d, nil
	}
	if approval == registry.ApprovedForThisCommit {
		x.note("approved locally at %s", shortSHA(sha))
	}
	x.note("ref %s → %s at %s", x.req.Ref, normalized, shortSHA(sha))

	// Gate 10 — the workspace, at the commit gate 8 just admitted. Checkout
	// rather than Acquire: Acquire resolves the ref again, and a second
	// resolution is a second answer — the window between them is precisely the
	// one an approval is supposed to close.
	ws, err := mirror.Checkout(ctx, x.run.ID, normalized, sha)
	if err != nil {
		return nil, registry.Decision{}, err
	}
	x.ws = ws
	x.onTeardown("workspace", func(context.Context) error { return ws.Remove() })
	x.note("checked out %s at %s", normalized, shortSHA(ws.SHA))

	// Gate 11 — the registry, off disk, at this commit.
	//
	// Since A42 this is two files rather than one: .latem/jobs.yaml if it
	// declares the job, and .github/workflows/*.yml otherwise. Both arrive as
	// a *registry.Registry, so nothing downstream of this line knows or cares
	// which one the job was written in — that is the point, and it is what
	// keeps §5's invariants true in one place instead of two.
	reg, src, err := workflow.Resolve(ws.Path, x.req.Job, x.workflowScope())
	if err != nil {
		return nil, registry.Decision{}, err
	}
	if !src.Native {
		// Worth a line in the run log: the job that ran is not in the file
		// most readers of this repository would look in, and a workflow's own
		// `on:` triggers are not what caused this run.
		x.note("job %q from %s", x.req.Job, src.Path)
	}
	return reg, registry.Decision{}, nil
}

// awaitApproval parks a run whose ref failed gate 8, the authoritative policy
// check (INV-5).
//
// This is now the only thing that parks a run. §11 draws the edge as "gate 4 /
// 8 ──▶ awaiting_approval", as though both gates sat in the same place; they do
// not, and only one of them can honestly park. Gate 4 runs before anything has
// been fetched, so it knows no commit, and a run parked without a commit is a
// run `latem approve` cannot bind an approval to (amendment A27). Gate 8 runs
// during preparing, after gate 9 has resolved one — and until amendment A17 the
// state table had no edge out of preparing to park it at all.
//
// The message carries the commit, because a parked run is a question put to a
// person and "approve refs/heads/feature/x" is not a question anyone can answer
// safely. The commits are appended here rather than composed in
// internal/registry, which decides policy from names and never sees a SHA.
func (x *execution) awaitApproval(ctx context.Context, dec registry.Decision) Result {
	expires := x.o.now().Add(x.o.approvalTTLFor(x.req.Repo))
	msg := dec.Msg
	switch {
	case x.run.ApprovedSHA != "" && x.sha != "":
		msg = fmt.Sprintf("%s (approved %s, now %s)",
			msg, shortSHA(x.run.ApprovedSHA), shortSHA(x.sha))
	case x.sha != "":
		msg = fmt.Sprintf("%s — this would build %s", msg, shortSHA(x.sha))
	}

	u := protocol.Update{
		ErrorMsg:  truncate(msg, MaxErrorMsgBytes),
		ExpiresAt: &expires,
	}
	// The transition also clears any approval on the row, in the store and in
	// the same statement that records the new commit: the operator approved a
	// different one and has to be asked again.
	if err := x.to(ctx, protocol.StateAwaitingApproval, u); err != nil {
		return x.raced(err)
	}
	x.note("awaiting approval: %s", msg)
	return Result{
		RunID:    x.run.ID,
		State:    protocol.StateAwaitingApproval,
		ErrorMsg: u.ErrorMsg,
	}
}

// requeue is gate 16's "wait, do not fail" (amendment A14).
//
// preparing → queued is a legal edge that the §11 diagram does not draw. A run
// gets as far as preparing, discovers its concurrency group is occupied, and
// goes back to the queue with a fresh queue_ttl. Its workspace is torn down by
// the deferred teardown and cloned again on the next attempt, which costs a few
// hundred kilobytes and keeps the model — one disposable clone per attempt —
// with no special case.
func (x *execution) requeue(ctx context.Context, group string) Result {
	holder, _ := x.o.groups.holder(group)
	x.note("concurrency group %q is held by run %s; returning to the queue", group, holder)

	expires := x.o.now().Add(x.o.queueTTL())
	u := protocol.Update{ExpiresAt: &expires}
	if x.runner != nil {
		u.Runner = x.runner.Name
	}
	if err := x.to(ctx, protocol.StateQueued, u); err != nil {
		return x.raced(err)
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := x.o.store.Enqueue(sctx, x.run.ID, 0); err != nil {
		x.note("could not re-enqueue: %v", err)
	}
	return Result{
		RunID:    x.run.ID,
		State:    protocol.StateQueued,
		Runner:   u.Runner,
		Requeued: true,
	}
}

// finish takes a run through collecting to its terminal state.
//
// `collecting` always runs once a workspace exists and the run ended in a way
// collecting can lead to — after failed, after timed_out, after cancelled —
// because a broken build's artifacts and a stopped build's partial logs are the
// diagnostic. Only a hard agent kill skips it, and that path produces
// interrupted at the next startup.
//
// A rejection is the exception, and it is the §11 machine's rather than this
// function's: collecting has no edge to rejected, because a run refused by an
// admission gate never had an environment and has nothing to collect. Those go
// straight from preparing.
func (x *execution) finish(ctx context.Context, out outcome) Result {
	// Reading the cancel flag and closing the door on cancellation are one
	// step: after this returns, a cancel is ignored (§11), so the answer must
	// be taken at the same instant the door closes. Taken separately, a cancel
	// landing between them would close a context nothing is listening to any
	// more and the run would report as though nobody had pressed Stop.
	cancelled, reason, actor := x.h.enterCollecting()
	if cancelled {
		out = cancelOutcome(out, reason, actor)
	}

	if x.ws == nil || !collectible(out.state) {
		return x.terminate(ctx, out)
	}
	if err := x.to(ctx, protocol.StateCollecting, protocol.Update{}); err != nil {
		return x.raced(err)
	}

	refs, out := x.collect(ctx, out)

	// §11 puts teardown inside collecting — "artifact globbing, cache flush,
	// teardown" — so it runs here, before the terminal transition, rather than
	// only on the way out. The deferred call in drive remains the guarantee for
	// every path that does not reach this line; teardown clears its own stack,
	// so the two do not double up.
	x.teardown(ctx)

	return x.terminateWith(ctx, out, refs)
}

// collectible reports whether the §11 machine has an edge from collecting to
// this terminal state. It is the table in internal/store read backwards, and
// keeping it as one predicate means a state added there fails one test rather
// than producing an illegal transition in four places.
func collectible(s protocol.RunState) bool {
	switch s {
	case protocol.StateSucceeded, protocol.StateFailed,
		protocol.StateTimedOut, protocol.StateCancelled:
		return true
	default:
		return false
	}
}

// collect globs the declared artifacts and copies them (§17).
//
// It runs under a context cancellation cannot reach, which is the mechanical
// half of "a cancel arriving during collecting is ignored": the person who
// pressed Stop wants the log and whatever was built, and tearing this down
// would throw away both.
func (x *execution) collect(ctx context.Context, out outcome) ([]protocol.ArtifactRef, outcome) {
	if x.resolved == nil || len(x.resolved.Artifacts) == 0 || x.o.artifacts == nil {
		return nil, out
	}

	cctx, cancel := x.o.teardownContext(ctx)
	defer cancel()

	decls := make([]artifact.Declaration, 0, len(x.resolved.Artifacts))
	for _, a := range x.resolved.Artifacts {
		decls = append(decls, artifact.Declaration{Path: a.Path, Retain: a.Retain.Duration()})
	}

	refs, err := x.o.artifacts.Collect(cctx, x.run.ID, x.ws.Path, decls)
	if err != nil {
		x.note("artifact collection failed: %v", x.redact(err.Error()))
		// A collection failure only decides the outcome of a run that was
		// otherwise going to succeed. Overwriting E_STEP with E_ARTIFACT would
		// hide why the build actually failed behind a consequence of it.
		if out.ok() {
			out = fail(err, protocol.ErrArtifact)
		}
		return refs, out
	}
	if len(refs) > 0 {
		x.note("collected %d artifact(s)", len(refs))
	}
	return refs, out
}

// terminate ends a run that never reached collecting.
func (x *execution) terminate(ctx context.Context, out outcome) Result {
	return x.terminateWith(ctx, out, nil)
}

func (x *execution) terminateWith(ctx context.Context, out outcome, refs []protocol.ArtifactRef) Result {
	out.msg = x.redact(out.msg)
	out.reason = x.redact(out.reason)

	u := x.o.terminalUpdate(out, refs)
	if x.runner != nil {
		u.Runner = x.runner.Name
	}
	if err := x.to(ctx, out.state, u); err != nil {
		return x.raced(err)
	}
	x.noteOutcome(out)

	// §15: the buffers are zeroed the moment the run is terminal. The string
	// copies in the executor's environment map cannot be zeroed — Go strings
	// are immutable — and are dropped with the run, which is the trade F10
	// recorded rather than the guarantee draft 01 could not keep.
	if x.sec != nil {
		x.sec.Zero()
	}

	return Result{
		RunID:     x.run.ID,
		State:     out.state,
		ErrorCode: out.code,
		ErrorMsg:  u.ErrorMsg,
		ExitCode:  out.exitCode,
		Runner:    u.Runner,
		Artifacts: refs,
	}
}

// raced reports a transition the store refused.
//
// It is not a failure of the run. The §11 machine is enforced in sqlite, so a
// refused edge means something else already moved the run — almost always a
// cancel that landed between two of this goroutine's steps — and the state it
// moved to is the true one. Re-reading it is more useful than inventing an
// error for a race the design expects.
func (x *execution) raced(err error) Result {
	if !errors.Is(err, store.ErrIllegalTransition) && !errors.Is(err, store.ErrNotFound) {
		x.note("state transition failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	res := Result{RunID: x.run.ID, State: x.run.State, ErrorMsg: err.Error()}
	if r, getErr := x.o.store.Get(ctx, x.run.ID); getErr == nil {
		res.State = r.State
		res.ErrorCode = r.ErrorCode
		res.ExitCode = r.ExitCode
		res.Runner = r.Runner
	}
	if x.sec != nil {
		x.sec.Zero()
	}
	return res
}

// to persists and reports one transition (§11).
//
// The context is detached from the run's on purpose. A cancelled run still has
// to record that it was cancelled, and a job whose wall clock expired still has
// to record the timeout; inheriting a context that is already closed would make
// the state machine unable to write down its own endings.
func (x *execution) to(ctx context.Context, to protocol.RunState, u protocol.Update) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()

	// Carried rather than sent on their own, so a run that is rejected at gate
	// 12 still records the commit it was rejected at. Re-sending them on every
	// later transition is a no-op in the store and costs nothing on the wire.
	if u.RefNormalized == "" {
		u.RefNormalized = x.refNormalized
	}
	if u.SHA == "" {
		u.SHA = x.sha
	}

	if err := x.o.transition(sctx, x.run.ID, to, u); err != nil {
		return err
	}
	x.run.State = to
	x.h.setPhase(to)
	return nil
}

// onTeardown registers a release for a resource that now exists.
func (x *execution) onTeardown(what string, fn func(context.Context) error) {
	x.teardowns = append(x.teardowns, teardownStep{what: what, fn: fn})
}

// teardown releases everything, most recent first, on every path out of drive.
//
// LIFO because the order resources were taken in is the order they depend on
// each other: the environment mounts the caches, the caches were locked for a
// workspace, and the workspace holds the artifacts collecting has by now
// copied out. It runs after collecting for the same reason.
func (x *execution) teardown(ctx context.Context) {
	tctx, cancel := x.o.teardownContext(ctx)
	defer cancel()

	for i := len(x.teardowns) - 1; i >= 0; i-- {
		t := x.teardowns[i]
		if err := t.fn(tctx); err != nil {
			x.note("tearing down %s: %v", t.what, x.redact(err.Error()))
		}
	}
	x.teardowns = nil
}

// noteOutcome writes the run's last line into its own log.
func (x *execution) noteOutcome(out outcome) {
	switch {
	case out.code != "":
		x.note("run %s: %s — %s", out.state, out.code, out.msg)
	case out.msg != "":
		x.note("run %s: %s", out.state, out.msg)
	default:
		x.note("run %s", out.state)
	}
}

// cancelOutcome replaces however a run ended with the fact that it was stopped.
//
// A cancel that took effect before collecting began is the reason the run
// ended, whatever the step loop reported on its way out — a step killed by
// SIGTERM exits non-zero, and calling that E_STEP would tell an operator their
// build is broken when they stopped it themselves.
func cancelOutcome(prev outcome, reason, actor string) outcome {
	out := outcome{
		state:  protocol.StateCancelled,
		reason: cancelReason(protocol.CancelRequest{Reason: reason, Actor: actor}),
	}
	out.msg = out.reason
	if prev.exitCode != nil {
		out.exitCode = prev.exitCode
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
