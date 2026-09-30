package run

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// resolveSecrets is the second half of gate 15: the names came out of the
// registry, the values come out of the §15 chain.
//
// Failure is not uniform, and amendment A16 is why. A name nobody configured
// is E_MISSING_SECRET — rejected, non-retryable, someone has to go and set it.
// A backend that is unusable is E_SECRET_BACKEND — failed, retryable — because
// the common instance is a Mac whose keychain is locked after a reboot,
// which resolves without anyone editing a commit. internal/secrets already
// tells the two apart, so this only has to not flatten them.
func (x *execution) resolveSecrets(ctx context.Context) (outcome, bool) {
	names := x.resolved.Secrets
	if len(names) == 0 {
		return outcome{}, true
	}
	if x.o.secrets == nil {
		return fail(errors.New("no secret provider is configured on this machine"),
			protocol.ErrSecretBackend), false
	}

	set, err := secrets.ResolveAll(ctx, x.o.secrets, x.red, names)
	if err != nil {
		return secretOutcome(err), false
	}
	x.sec = set

	// Every value is registered with the redactor before this returns — that
	// is a property of secrets.Set's type, not a rule anyone has to remember
	// — so warnings can be surfaced without them leaking. §16 requires a warn
	// on a short value naming the secret and never the value.
	for _, w := range set.Warnings() {
		x.note("secret %s", w)
	}
	x.note("resolved %d secret(s): %v", set.Len(), set.Names())
	return outcome{}, true
}

// checkDisk is gate 17: the selected runner's own floor.
//
// Gate 5 could only enforce the weakest floor on the machine, because the
// runner is not chosen until gate 13. Checking twice costs one statfs and
// prevents both a false rejection — a docker job refused against the Tart
// runner's 150 GB — and a build that dies at 80% with a full disk.
func (x *execution) checkDisk(ctx context.Context) error {
	if x.o.caches == nil || x.runner == nil || x.runner.MinFreeGB <= 0 {
		return nil
	}
	free, err := x.o.caches.FreeGB(ctx)
	if err != nil {
		// Not being able to measure is not the same as being full. Gate 5 has
		// already passed against the weakest floor; refusing here on an
		// unanswerable question would fail builds on a healthy machine.
		x.note("could not measure free disk for runner %q: %v", x.runner.Name, err)
		return nil
	}
	if free < x.runner.MinFreeGB {
		return &cache.Error{
			Code: protocol.ErrDisk,
			Msg: fmt.Sprintf("runner %q needs %.1f GB free, %.1f GB available",
				x.runner.Name, x.runner.MinFreeGB, free),
		}
	}
	return nil
}

// acquireCaches is gate 18.
//
// The mount target comes from the registry's caches[].path and travels with
// the request (amendment A11). A cache manager cannot read jobs.yaml, and
// draft 02's name-only Acquire returned mounts with an empty target — silent
// at every layer, and the only symptom is a build that stays cold forever.
func (x *execution) acquireCaches(ctx context.Context) ([]executor.CacheMount, error) {
	if len(x.resolved.Caches) == 0 {
		return nil, nil
	}
	if x.o.caches == nil {
		return nil, &cache.Error{
			Code: protocol.ErrCache,
			Msg:  "no cache manager is configured on this machine",
		}
	}

	reqs := make([]cache.Request, 0, len(x.resolved.Caches))
	names := make([]string, 0, len(x.resolved.Caches))
	for _, c := range x.resolved.Caches {
		reqs = append(reqs, cache.Request{Name: c.Name, Target: c.Path})
		names = append(names, c.Name)
	}

	mounts, err := x.o.caches.Acquire(ctx, x.runner.Name, reqs)
	// The release is registered even when Acquire failed: §14 documents
	// Release as idempotent and safe after a failed Acquire precisely because
	// a partial acquisition leaves locks held, and the caller cannot tell how
	// far it got.
	runner := x.runner.Name
	x.onTeardown("caches "+fmt.Sprint(names), func(tctx context.Context) error {
		return x.o.caches.Release(tctx, runner, names)
	})
	if err != nil {
		return nil, err
	}
	x.note("mounted %d cache(s): %v", len(mounts), names)
	return mounts, nil
}

// prepareEnv assembles the run environment and provisions the executor.
func (x *execution) prepareEnv(ctx context.Context, mounts []executor.CacheMount) error {
	if x.runner.Executor == nil {
		return fmt.Errorf("runner %q has no executor", x.runner.Name)
	}

	image := x.resolved.Image
	if image == "" {
		image = x.runner.DefaultImage
	}

	// The action cache, if this machine has one. Created rather than assumed:
	// docker binds it into the container, and a bind of a path that is not
	// there fails the run rather than the fetch (A42 phase 5). An empty
	// directory costs nothing on a machine that never fetched an action.
	actionsDir, err := x.actionsDir()
	if err != nil {
		return err
	}

	spec := executor.Spec{
		RunID:      x.run.ID,
		Workspace:  x.ws.Path,
		Image:      image,
		Env:        x.buildEnv(),
		Secrets:    x.secretNames(),
		Caches:     mounts,
		ActionsDir: actionsDir,
		Labels: map[string]string{
			"latem.run-id": x.run.ID,
			"latem.repo":   x.run.Repo,
			"latem.job":    x.resolved.JobName,
		},
		Resources: x.runner.Resources,
	}

	env, err := x.runner.Executor.Prepare(ctx, spec)
	if err != nil {
		return err
	}
	x.env = env
	x.onTeardown("environment", env.Close)
	return nil
}

// actionsDir is the host directory holding fetched actions, created if needed.
func (x *execution) actionsDir() (string, error) {
	if x.o.cfg == nil || x.o.cfg.Paths.Actions == "" {
		return "", nil
	}
	dir := x.o.cfg.Paths.Actions
	if err := os.MkdirAll(dir, actionsDirMode); err != nil {
		return "", &registry.Error{
			Code: protocol.ErrExecutor,
			Msg:  "preparing the action cache",
			Err:  err,
		}
	}
	return dir, nil
}

// actionsDirMode is tight because the directory holds code that is about to
// run, and matches internal/workflow/actions' own.
const actionsDirMode = 0o700
