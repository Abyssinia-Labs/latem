package registry

import (
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// MaxInputKeys bounds gate 3. A job declaring more than a handful of inputs is
// unusual; sixty-four is generous and still cheap to reject beyond.
const MaxInputKeys = 64

// RepoPolicy is the subset of a repo's configuration that admission needs.
//
// Declared here rather than imported from internal/config so this package
// stays free of dependencies it does not need. WS-04 adapts its own types to
// this at the call site, which also keeps the config schema free to change
// shape without touching the gate chain.
type RepoPolicy struct {
	// AllowedRefs are raw patterns as written in config, before [ExpandPolicy].
	AllowedRefs []string
	// RequireApprovalOtherwise sends non-matching refs to awaiting_approval.
	// When false, a non-matching ref is rejected outright.
	RequireApprovalOtherwise bool
}

// Policy is everything gates 1-5 consult.
type Policy struct {
	Repos map[string]RepoPolicy
	// FreeGB and MinFreeGB implement gate 5. MinFreeGB is the weakest floor
	// across enabled runners; the selected runner's own floor is re-checked at
	// gate 17, once there is a runner to check against.
	FreeGB    float64
	MinFreeGB float64
}

// Decision is the outcome of a gate chain. A zero Decision means proceed.
type Decision struct {
	State protocol.RunState
	Code  protocol.ErrorCode
	Msg   string
}

// OK reports whether the run may continue to the next stage.
func (d Decision) OK() bool { return d.State == "" }

// Error renders the decision for a log line or Update.ErrorMsg. It is already
// safe to show: nothing here interpolates a secret, and the ref and input
// values it quotes have passed validation.
func (d Decision) Error() string {
	if d.OK() {
		return ""
	}
	return fmt.Sprintf("%s: %s", d.Code, d.Msg)
}

func reject(code protocol.ErrorCode, format string, args ...any) Decision {
	return Decision{State: protocol.StateRejected, Code: code, Msg: fmt.Sprintf(format, args...)}
}

func needsApproval(format string, args ...any) Decision {
	return Decision{State: protocol.StateAwaitingApproval, Msg: fmt.Sprintf(format, args...)}
}

// rejectErr turns an error from this package into a rejection, keeping its
// code when it carries one. The fallback covers an error arriving from
// somewhere unexpected: a decision with no code would leave a control plane
// unable to say anything useful about why a run failed.
func rejectErr(err error, fallback protocol.ErrorCode) Decision {
	code := CodeOf(err)
	if code == "" {
		code = fallback
	}
	return Decision{State: protocol.StateRejected, Code: code, Msg: Message(err)}
}

// PreAdmit runs gates 1-5 of RUNNER_SPEC §10: everything decidable without
// touching the network or the filesystem.
//
// Ordering is the whole point. A hostile request is rejected after a map
// lookup and a regexp, having caused no fetch, no clone, no container, and no
// process. Gate 6 is the first thing that talks to the outside world.
//
// It returns either a proceed or a rejection, never awaiting_approval. See gate
// 4 below for why parking is not something this function can honestly do.
func PreAdmit(req protocol.RunRequest, p Policy) Decision {
	// Gate 1 — repo lookup. An exact map hit against local config. No path is
	// ever constructed from this value; a repo absent here cannot be built,
	// which is what makes the config file an allowlist rather than a hint.
	repo, ok := p.Repos[req.Repo]
	if !ok {
		return reject(protocol.ErrUnknownRepo,
			"repo %q is not configured on this machine", req.Repo)
	}

	// Gate 2 — ref syntax (INV-3).
	if err := ValidateRef(req.Ref); err != nil {
		return rejectErr(err, protocol.ErrInvalidRef)
	}

	// Gate 3 — input sanity. Shape only; the declarations that give these
	// meaning live in a registry that does not exist until a commit is checked
	// out, so the authoritative pass is gate 14.
	if len(req.Inputs) > MaxInputKeys {
		return reject(protocol.ErrBadInput,
			"%d inputs supplied, limit %d", len(req.Inputs), MaxInputKeys)
	}
	for _, k := range sortedKeys(req.Inputs) {
		if !identPattern.MatchString(k) {
			return reject(protocol.ErrBadInput,
				"input name %q must match %s", k, identPattern)
		}
		if len(req.Inputs[k]) > MaxInputValueBytes {
			return reject(protocol.ErrBadInput,
				"input %q is %d bytes, limit %d", k, len(req.Inputs[k]), MaxInputValueBytes)
		}
	}

	// Gate 4 — conservative ref policy (INV-5). See [CouldMatchPolicy] for why
	// an optimistic answer here is sound.
	//
	// It rejects and never parks (amendment A27). Parking is what asks a human
	// to approve a commit, and no commit is known here: nothing has been
	// fetched, so the ref has not been resolved and cannot be. A run parked at
	// this gate would carry no sha, `latem approve` would have nothing to pin,
	// and the only way out would be an approval that means "whatever this
	// branch points at whenever the build eventually starts" — which is the
	// force-push hole approval exists to close. So when approval is possible
	// the run proceeds and the authoritative gate 8 parks it, after gate 9 has
	// resolved the commit the operator will be asked about.
	//
	// The cheap half of this gate is kept exactly where the answer is knowable
	// with no I/O: a repo that does not permit approval at all is refused here,
	// having caused no fetch.
	expanded := ExpandPolicy(repo.AllowedRefs)
	if !CouldMatchPolicy(req.Ref, expanded) && !repo.RequireApprovalOtherwise {
		return reject(protocol.ErrInvalidRef,
			"ref %q is outside allowed_refs for %q and approval is disabled",
			req.Ref, req.Repo)
	}

	// Gate 5 — disk headroom against the weakest runner floor.
	if p.MinFreeGB > 0 && p.FreeGB < p.MinFreeGB {
		return reject(protocol.ErrDisk,
			"%.1f GB free, need at least %.1f GB", p.FreeGB, p.MinFreeGB)
	}

	return Decision{}
}

// Approval is what a local `latem approve` means to gate 8 (amendment A27).
//
// It is three-valued rather than a bool because "approved, but not for the
// commit this run is about to build" is a distinct answer that deserves a
// distinct message: the operator looked at something, and it is no longer what
// they looked at.
//
// The comparison itself is deliberately not done here. This package decides
// policy from names — it never sees a SHA, has no mirror to resolve one
// against, and would have to be handed two of them to compare. Which commit was
// approved and which one the ref resolves to now are facts the orchestrator
// already holds; [ApprovalFor] turns that pair into this. Keeping it that way
// means gate 8 stays a pure function of the policy and one enum, and the code
// that could get a commit comparison wrong is one line with its own test.
type Approval uint8

const (
	// NotApproved is the ordinary case: nobody has approved this run.
	NotApproved Approval = iota
	// ApprovedForThisCommit means an operator approved exactly the commit the
	// ref resolves to now.
	ApprovedForThisCommit
	// ApprovedForAnotherCommit means an operator approved this run, and the ref
	// has moved since. It is not a weaker form of approval; it is a refusal
	// with a reason.
	ApprovedForAnotherCommit
)

// ApprovalFor classifies a recorded approval against the commit a run is about
// to build.
//
// Empty is never a match on either side. An approved_sha of "" is a run nobody
// approved, and a resolved SHA of "" is a caller that has not resolved one yet
// — treating those as equal would admit an unapproved run on a pair of empty
// strings, which is the kind of bug that reads as obviously fine.
func ApprovalFor(approvedSHA, resolvedSHA string) Approval {
	switch {
	case approvedSHA == "":
		return NotApproved
	case resolvedSHA != "" && approvedSHA == resolvedSHA:
		return ApprovedForThisCommit
	default:
		return ApprovedForAnotherCommit
	}
}

// AdmitRef runs gate 8, the authoritative policy check, against a ref that has
// already been normalized by [NormalizeRef] and resolved to a commit.
//
// Gate 4 asked whether any expansion *could* match. This asks whether the one
// that actually exists *does*. Both are needed: the first keeps hostile
// requests cheap, the second keeps the answer correct.
//
// A ref outside the policy is admitted only by an approval bound to the commit
// in hand. INV-5 exists because "a ref can point at a branch a contributor
// controls" — so an approval that granted the *ref* would be defeated by
// pushing something benign, asking for approval, and force-pushing before the
// build starts. That is a time-of-check-to-time-of-use hole that defeats the
// invariant entirely, which is why [ApprovedForAnotherCommit] parks the run
// again rather than being rounded up to a yes.
func AdmitRef(runID, repoName, normalized string, repo RepoPolicy, approval Approval) Decision {
	if MatchPolicy(normalized, ExpandPolicy(repo.AllowedRefs)) {
		return Decision{}
	}
	if approval == ApprovedForThisCommit {
		return Decision{}
	}
	if !repo.RequireApprovalOtherwise {
		return reject(protocol.ErrInvalidRef,
			"ref %q is outside allowed_refs for %q and approval is disabled",
			normalized, repoName)
	}
	if approval == ApprovedForAnotherCommit {
		return needsApproval(
			"ref %q moved since it was approved; approval covers one commit, "+
				"so this run needs a fresh `latem approve %s`",
			normalized, runID)
	}
	return needsApproval(
		"ref %q is outside allowed_refs for %q; approve locally with `latem approve %s`",
		normalized, repoName, runID)
}

// Resolved is an admitted job, ready for execution. Everything a run needs and
// nothing a broker supplied beyond names.
type Resolved struct {
	JobName string
	Job     *Job
	Image   string
	Timeout time.Duration
	// Inputs are coerced and canonical: every declared name present, defaults
	// filled, types enforced.
	Inputs           map[string]string
	Steps            []ResolvedStep
	Secrets          []string
	Caches           []CacheDecl
	Artifacts        []ArtifactDecl
	ConcurrencyGroup string
	Shell            string
	// RequiresExecutor is the executor gate 13 held the runner to, or "".
	RequiresExecutor string
	// RequiresIsolation records that gate 13 held the runner to confining the
	// job's steps, so a log can say why it landed where it did.
	RequiresIsolation bool
}

// ResolvedStep is a step with its environment already substituted.
//
// Already substituted, except for Late: those values are resolved by the run
// loop against a [StepScope] immediately before the step runs, and merged over
// Env then. If is evaluated at the same moment and decides whether the step
// runs at all.
type ResolvedStep struct {
	ID   string
	Name string
	Run  string
	// Shell, ContinueOnError and ActionUse are carried from [Step] unchanged
	// (A50, A51).
	Shell           string
	ContinueOnError bool
	ActionUse       string
	Env             map[string]string
	Late            map[string]LateValue
	If              Condition
	// Container is set for a container step, in which case Run is empty.
	Container *ContainerStep
	// Artifact is set for a step the run loop performs itself (A49).
	Artifact *ArtifactStep
	// Timeout of zero means "whatever remains of the job budget". Resolving
	// that needs a clock and a start time, so it belongs to the orchestrator
	// rather than here.
	Timeout time.Duration
}

// Admit runs gates 12-15 of RUNNER_SPEC §10 against a parsed registry.
//
// Gates 6-11 (fetch, normalize, policy, resolve, clone, read) happen between
// [PreAdmit] and this call and belong to internal/source and the orchestrator.
// By the time this runs, the registry argument came off disk at a pinned
// commit — never from a broker (INV-5).
func (r *Registry) Admit(req protocol.RunRequest, runnerLabels []string) (*Resolved, Decision) {
	return r.AdmitOn(req, Runner{Labels: runnerLabels})
}

// AdmitOn is [Registry.Admit] for a caller that knows the runner's executor as
// well as its labels, which since A44 gate 13 needs: a job with a container
// step can only land on a docker runner, and a machine that has none should
// hear so here rather than from a build.
func (r *Registry) AdmitOn(req protocol.RunRequest, runner Runner) (*Resolved, Decision) {
	runnerLabels := runner.Labels
	// Gate 12 — job lookup.
	job, ok := r.Jobs[req.Job]
	if !ok {
		return nil, reject(protocol.ErrUnknownJob,
			"job %q is not in %s at this commit (known: %v)", req.Job, Path, sortedKeys(r.Jobs))
	}

	// Gate 13 — label match. The runner must carry every label the job asks
	// for; extra labels on the runner are fine.
	if missing := missingLabels(job.RunsOn, runnerLabels); len(missing) > 0 {
		return nil, reject(protocol.ErrNoRunner,
			"job %q needs labels %v; this runner is missing %v", req.Job, job.RunsOn, missing)
	}

	// Gate 13, second half (A44) — the executor. Labels say what a machine
	// is; this says what it can run. Checked after the labels so that a
	// machine which is simply the wrong one is told that first.
	if need := job.RequiredExecutor(); need != "" && runner.Executor != need {
		have := runner.Executor
		if have == "" {
			have = "unknown"
		}
		return nil, reject(protocol.ErrNoRunner,
			"job %q has a container step, which needs a %s runner; this runner's executor is %s",
			req.Job, need, have)
	}

	// Gate 13, third part (A47) — isolation. A job carrying third-party code
	// may only land on an executor that confines it. The operator's opt-out
	// was applied at compile time, so a job that still asks for this has not
	// been excused from it.
	if job.RequiresIsolation && !runner.Isolates {
		return nil, reject(protocol.ErrNoRunner,
			"job %q runs a third-party action, which needs an executor that confines it to the "+
				"workspace — this runner's %s executor runs steps as the agent's user. Use a docker "+
				"runner, or set [actions] allow_unisolated on a machine where that is acceptable",
			req.Job, orUnknown(runner.Executor))
	}

	// Gate 14 — typed input validation.
	inputs, err := job.CoerceInputs(req.Inputs)
	if err != nil {
		return nil, rejectErr(err, protocol.ErrBadInput)
	}

	// Gate 15 — secrets. Only names are known here; resolution is
	// internal/secrets, and an unresolvable name becomes E_MISSING_SECRET
	// before the first step runs rather than forty minutes into a build.
	caches := make([]CacheDecl, 0, len(job.Caches))
	for _, name := range job.Caches {
		c, found := r.Cache(name)
		if !found {
			// Unreachable: Validate rejects undeclared cache references.
			return nil, reject(protocol.ErrBadRegistry, "cache %q is not declared", name)
		}
		caches = append(caches, c)
	}

	steps := make([]ResolvedStep, 0, len(job.Steps))
	for _, s := range job.Steps {
		env := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			env[k] = substitute(v, inputs)
		}
		steps = append(steps, ResolvedStep{
			ID:              s.ID,
			Name:            s.Name,
			Run:             s.Run,
			Shell:           s.Shell,
			ContinueOnError: s.ContinueOnError,
			ActionUse:       s.ActionUse,
			Env:             env,
			Late:            s.Late,
			If:              s.If,
			Container:       s.Container,
			Artifact:        s.Artifact,
			Timeout:         s.Timeout.Duration(),
		})
	}

	return &Resolved{
		JobName:           req.Job,
		Job:               job,
		Image:             job.Image,
		Timeout:           r.JobTimeout(job),
		Inputs:            inputs,
		Steps:             steps,
		Secrets:           job.Secrets,
		Caches:            caches,
		Artifacts:         job.Artifacts,
		ConcurrencyGroup:  job.ConcurrencyGroup,
		Shell:             r.Defaults.Shell,
		RequiresExecutor:  job.RequiredExecutor(),
		RequiresIsolation: job.RequiresIsolation,
	}, Decision{}
}

func missingLabels(required, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, l := range have {
		set[l] = true
	}
	var missing []string
	for _, l := range required {
		if !set[l] {
			missing = append(missing, l)
		}
	}
	return missing
}

// orUnknown names an executor a caller did not supply. The gate fails closed
// on it, so the message has to be usable when it happens.
func orUnknown(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}
