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
	expanded := ExpandPolicy(repo.AllowedRefs)
	if !CouldMatchPolicy(req.Ref, expanded) {
		if !repo.RequireApprovalOtherwise {
			return reject(protocol.ErrInvalidRef,
				"ref %q is outside allowed_refs for %q and approval is disabled",
				req.Ref, req.Repo)
		}
		return needsApproval(
			"ref %q is outside allowed_refs for %q; approve locally with `latem approve %s`",
			req.Ref, req.Repo, req.ID)
	}

	// Gate 5 — disk headroom against the weakest runner floor.
	if p.MinFreeGB > 0 && p.FreeGB < p.MinFreeGB {
		return reject(protocol.ErrDisk,
			"%.1f GB free, need at least %.1f GB", p.FreeGB, p.MinFreeGB)
	}

	return Decision{}
}

// AdmitRef runs gate 8, the authoritative policy check, against a ref that has
// already been normalized by [NormalizeRef].
//
// Gate 4 asked whether any expansion *could* match. This asks whether the one
// that actually exists *does*. Both are needed: the first keeps hostile
// requests cheap, the second keeps the answer correct.
func AdmitRef(runID, repoName, normalized string, repo RepoPolicy) Decision {
	if MatchPolicy(normalized, ExpandPolicy(repo.AllowedRefs)) {
		return Decision{}
	}
	if !repo.RequireApprovalOtherwise {
		return reject(protocol.ErrInvalidRef,
			"ref %q is outside allowed_refs for %q and approval is disabled",
			normalized, repoName)
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
}

// ResolvedStep is a step with its environment already substituted.
type ResolvedStep struct {
	Name string
	Run  string
	Env  map[string]string
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
			Name:    s.Name,
			Run:     s.Run,
			Env:     env,
			Timeout: s.Timeout.Duration(),
		})
	}

	return &Resolved{
		JobName:          req.Job,
		Job:              job,
		Image:            job.Image,
		Timeout:          r.JobTimeout(job),
		Inputs:           inputs,
		Steps:            steps,
		Secrets:          job.Secrets,
		Caches:           caches,
		Artifacts:        job.Artifacts,
		ConcurrencyGroup: job.ConcurrencyGroup,
		Shell:            r.Defaults.Shell,
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
