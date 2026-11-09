package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The files a checkout hands the environment through [executor.Env.PutTemp].
const (
	checkoutCredentialsFile = "git-credentials.config"
	checkoutPackFile        = "checkout.pack"
)

// checkoutTarget is where a checkout step moves the working tree, and why it
// may.
type checkoutTarget struct {
	// ref is the fully-qualified ref the step's `ref:` normalized to, or the
	// run's own; empty for a commit named by its id.
	ref string
	sha string
	// vouch is the policy ref whose history holds sha; empty for the run's
	// own commit, which needs no other reason.
	vouch string
	pack  []byte
}

// errCheckoutRefused is a checkout the policy does not allow, or a ref that
// names nothing: the workflow's to fix, so E_STEP, never a retry.
var errCheckoutRefused = errors.New("checkout refused")

// runCheckoutStep performs `uses: actions/checkout` (A53).
//
// The agent's half runs on the host, in the mirror it owns: resolve the ref
// against a freshly fetched mirror, decide whether the policy admits the
// commit, pack whatever the workspace lacks, and write the credential. The
// environment's half — everything that reads or writes the workspace's .git
// — runs as the step's own process ([source.SwitchScript]), because every
// earlier step could have rewritten that .git, and git on the host would run
// what it found there.
//
// The job's definition does not move: it was compiled from the run's commit
// at gate 11 and stays that, and so do github.sha and LATEM_SHA (INV-5). The
// log names both commits, so a run is reproducible from what it records.
func (x *execution) runCheckoutStep(
	ctx context.Context, s registry.ResolvedStep, label string, sc registry.StepScope, budget time.Duration,
) outcome {
	c := s.Checkout
	written := c.Ref
	if c.RefLate != nil {
		v, err := c.RefLate.Resolve(sc)
		if err != nil {
			return fail(&registry.Error{Code: protocol.ErrBadRegistry, Msg: "checkout: ref", Err: err},
				protocol.ErrBadRegistry)
		}
		written = strings.TrimSpace(v)
	}

	target, err := x.checkoutTarget(ctx, written)
	if err != nil {
		if errors.Is(err, errCheckoutRefused) {
			return outcome{state: protocol.StateFailed, code: protocol.ErrStep, msg: err.Error()}
		}
		return fail(err, protocol.ErrSource)
	}
	x.noteCheckout(written, target)
	if len(c.Sparse) > 0 {
		mode := "cone mode"
		if !c.SparseCone {
			mode = "not cone mode"
		}
		x.note("checkout: sparse, %s, leaving only %s", mode, strings.Join(c.Sparse, ", "))
	}

	sw := source.Switch{
		SHA: target.sha, Clean: c.Clean, Tracking: written != "",
		Sparse: c.Sparse, SparseCone: c.SparseCone,
	}
	if b, ok := strings.CutPrefix(target.ref, "refs/heads/"); ok {
		sw.Branch = b
	}
	if len(target.pack) > 0 {
		if sw.Pack, err = x.env.PutTemp(ctx, checkoutPackFile, target.pack); err != nil {
			return fail(err, protocol.ErrExecutor)
		}
	}
	if sw.Remote, sw.Include, err = x.checkoutCredential(ctx, c, sc); err != nil {
		return fail(err, protocol.ErrExecutor)
	}

	env, err := sw.Env()
	if err != nil {
		return fail(err, protocol.ErrSource)
	}
	script := registry.ResolvedStep{
		Name:    s.Name,
		Run:     source.SwitchScript,
		Shell:   string(executor.ShellGitHubSh),
		Timeout: s.Timeout,
	}
	out, _ := x.runStep(ctx, script, label, budget, env)
	return out
}

// checkoutTarget resolves a checkout's `ref:` and decides whether the
// repository's policy admits where it leads (A53).
//
// Reachability, not a name: the commit must be the run's own or lie in the
// history of a ref allowed_refs admits, in the mirror as just fetched. That
// lets through `ref: main` after an earlier job pushed to it, a rollback tag
// on main, and `needs.bump.outputs.sha`; it refuses a commit only a branch
// the policy does not name holds, whatever the workflow calls it. There is no
// parking: a running job cannot go back to awaiting_approval, and an approval
// names only the run's own commit.
func (x *execution) checkoutTarget(ctx context.Context, written string) (checkoutTarget, error) {
	if written == "" {
		return checkoutTarget{ref: x.refNormalized, sha: x.sha}, nil
	}
	policy, repoCfg, ok := x.o.repoPolicy(x.req.Repo)
	if !ok {
		return checkoutTarget{}, fmt.Errorf("%w: repo %q is no longer configured on this machine", errCheckoutRefused, x.req.Repo)
	}
	if x.o.src == nil {
		return checkoutTarget{}, errors.New("run: no source manager configured")
	}
	// Fetched again, not reused from gate 6: `ref: main` exists to see a
	// commit an earlier job of this run pushed after this one was admitted.
	mirror, err := x.o.src.Fetch(ctx, source.Repo{Key: x.req.Repo, URL: repoCfg.URL, Token: repoCfg.FetchToken(x.req.FetchToken)})
	if err != nil {
		return checkoutTarget{}, err
	}

	var t checkoutTarget
	if registry.IsCommitID(written) {
		has, err := mirror.HasCommit(ctx, written)
		if err != nil {
			return checkoutTarget{}, err
		}
		if !has {
			return checkoutTarget{}, fmt.Errorf("%w: checkout: commit %s is not in %s", errCheckoutRefused, written, x.req.Repo)
		}
		t.sha = written
	} else {
		// As gate 7 normalizes a run's ref, ambiguity included.
		if t.ref, err = registry.NormalizeRef(ctx, mirror, written); err != nil {
			var re *registry.Error
			if errors.As(err, &re) && re.Code != protocol.ErrSource {
				return checkoutTarget{}, fmt.Errorf("%w: checkout: ref %q: %w", errCheckoutRefused, written, err)
			}
			return checkoutTarget{}, err
		}
		if t.sha, err = mirror.Resolve(ctx, t.ref); err != nil {
			return checkoutTarget{}, err
		}
	}

	if t.sha != x.sha {
		refs, err := mirror.ContainingRefs(ctx, t.sha, registry.PolicyPrefixes(policy.AllowedRefs))
		if err != nil {
			return checkoutTarget{}, err
		}
		vouch, ok := registry.AdmitCheckout(t.sha, x.sha, refs, policy.AllowedRefs)
		if !ok {
			return checkoutTarget{}, fmt.Errorf("%w: checkout: %s (%s) is not in the history of any ref allowed_refs admits "+
				"for %s, so it is code the policy does not trust; a checkout may reach only the run's commit or one "+
				"reachable from %v (A53)", errCheckoutRefused, shortSHA(t.sha), written, x.req.Repo, policy.AllowedRefs)
		}
		t.vouch = vouch
	}
	if t.pack, err = mirror.PackSince(ctx, t.sha, x.sha); err != nil {
		return checkoutTarget{}, err
	}
	return t, nil
}

// noteCheckout records what the checkout resolved, beside the run's commit,
// so the log answers "what did this job actually build".
func (x *execution) noteCheckout(written string, t checkoutTarget) {
	switch {
	case written == "":
		x.note("checkout: the run's commit %s (%s)", shortSHA(t.sha), t.ref)
	case t.sha == x.sha:
		x.note("checkout: ref %s → %s at %s, the run's commit", written, orCommit(t.ref), shortSHA(t.sha))
	default:
		x.note("checkout: ref %s → %s at %s (the run's commit is %s; admitted by %s)",
			written, orCommit(t.ref), shortSHA(t.sha), shortSHA(x.sha), t.vouch)
	}
}

func orCommit(ref string) string {
	if ref == "" {
		return "a commit"
	}
	return ref
}

// checkoutCredential makes the job's token the workspace's credential, unless
// the step says persist-credentials: false (A53). It returns the URL origin
// is pinned to and the file the workspace includes, either of which may be
// empty.
//
// The token is registered with the redactor, in every form the file holds,
// before the file exists. A second checkout that does not persist empties
// the file a first one wrote, as well as dropping the include.
func (x *execution) checkoutCredential(
	ctx context.Context, c *registry.CheckoutStep, sc registry.StepScope,
) (remote, include string, err error) {
	slug, isGitHub := x.githubRepository()
	if !isGitHub {
		if c.PersistCredentials {
			x.note("checkout: %s is not a GitHub repository, so the workspace gets no credential", x.req.Repo)
		}
		return "", "", nil
	}
	remote = githubServerURL + "/" + slug + ".git"

	if !c.PersistCredentials {
		if x.credentialWritten {
			if _, err := x.env.PutTemp(ctx, checkoutCredentialsFile, nil); err != nil {
				return "", "", err
			}
		}
		return remote, "", nil
	}
	token := x.req.Token
	if c.Token != nil {
		v, err := c.Token.Resolve(sc)
		if err != nil {
			return "", "", &registry.Error{Code: protocol.ErrBadRegistry, Msg: "checkout: token", Err: err}
		}
		token = strings.TrimSpace(v)
	}
	if token == "" {
		x.note("checkout: this job has no token, so the workspace gets no credential")
		return remote, "", nil
	}
	if x.red != nil {
		for _, form := range source.CredentialSecrets(token) {
			x.red.Register([]byte(form))
		}
	}
	cfg, err := source.CredentialConfig(remote, token)
	if err != nil {
		return "", "", err
	}
	if include, err = x.env.PutTemp(ctx, checkoutCredentialsFile, cfg); err != nil {
		return "", "", err
	}
	x.credentialWritten = true
	return remote, include, nil
}
