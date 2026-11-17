package run

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Abyssinia-Labs/latem/internal/repovars"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
)

// GitHub's public endpoints; see [workflow.GitHubServerURL].
const (
	githubServerURL  = workflow.GitHubServerURL
	githubAPIURL     = workflow.GitHubAPIURL
	githubGraphQLURL = workflow.GitHubGraphQLURL
)

// githubRepository is the repository as GitHub names it, owner/name (A50).
//
// The config key is the fallback, which is what github.repository was before
// A50: a repository reached by a local path has no owner/name to read, and
// the key is still more use to a step than nothing. ok reports which one it is.
func (x *execution) githubRepository() (slug string, ok bool) {
	if r, found := x.o.config().Repos[x.req.Repo]; found {
		if s, isGitHub := r.GitHubRepository(); isGitHub {
			return s, true
		}
	}
	return x.req.Repo, false
}

// refParts splits a fully-qualified ref into GitHub's ref_name and ref_type.
func refParts(ref string) (name, kind string) {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return strings.TrimPrefix(ref, "refs/heads/"), "branch"
	case strings.HasPrefix(ref, "refs/tags/"):
		return strings.TrimPrefix(ref, "refs/tags/"), "tag"
	}
	return ref, ""
}

// githubEnv is the environment GitHub gives a job, for a job compiled from a
// workflow (A50).
//
// Not the workspace or the temporary directory: only an executor knows where
// those are as a step sees them, so each executor sets $GITHUB_WORKSPACE and
// $RUNNER_TEMP itself. Not GITHUB_EVENT_PATH or GITHUB_TOKEN either; both
// need an event and a GitHub identity, which latem does not have yet.
//
// GITHUB_ACTIONS is "true" because the contract is GitHub's: a workflow, and
// the tools it calls, test it to decide whether they are in CI, and a workflow
// adopted onto latem is.
func (x *execution) githubEnv() map[string]string {
	slug, isGitHub := x.githubRepository()
	owner := ""
	if isGitHub {
		owner, _, _ = strings.Cut(slug, "/")
	}
	refName, refType := refParts(x.refNormalized)
	facts := runnerFacts(x.runner)

	env := map[string]string{
		"CI":                      "true",
		"GITHUB_ACTIONS":          "true",
		"GITHUB_REPOSITORY":       slug,
		"GITHUB_REPOSITORY_OWNER": owner,
		"GITHUB_SHA":              x.sha,
		"GITHUB_REF":              x.refNormalized,
		"GITHUB_REF_NAME":         refName,
		"GITHUB_REF_TYPE":         refType,
		"GITHUB_EVENT_NAME":       eventName(x.req.Origin.Kind),
		"GITHUB_ACTOR":            x.req.Origin.Actor,
		"GITHUB_JOB":              x.req.Job,
		"GITHUB_RUN_ID":           x.run.ID,
		"GITHUB_RUN_ATTEMPT":      "1",
		"GITHUB_SERVER_URL":       githubServerURL,
		"GITHUB_API_URL":          githubAPIURL,
		"GITHUB_GRAPHQL_URL":      githubGraphQLURL,
		"RUNNER_OS":               workflow.RunnerOS(facts.OS),
		"RUNNER_ARCH":             workflow.RunnerArch(facts.Arch),
	}
	if x.workflow != nil {
		env["GITHUB_WORKFLOW"] = x.workflow.Workflow
		// A called workflow's job is its own id there, as on GitHub (A82).
		if x.workflow.Job != "" {
			env["GITHUB_JOB"] = x.workflow.Job
		}
	}
	// The same number as $LATEM_BUILD_NUMBER, and absent for the same reason
	// when the row predates it: a workflow falling back to its own number
	// cannot do so on a variable that is set.
	if x.run.Number > 0 {
		env["GITHUB_RUN_NUMBER"] = strconv.FormatInt(x.run.Number, 10)
	}
	if x.isWorkflowRun() {
		x.workflowRunEnv(env)
	}
	if facts.Name != "" {
		env["RUNNER_NAME"] = facts.Name
	}
	for k, v := range env {
		if v == "" {
			delete(env, k)
		}
	}
	return env
}

// toolCache is the runner's persistent tool cache for a job compiled from a
// workflow (A51): a volume of its own for docker, a directory under the
// agent's state for an executor on the host. A jobs.yaml job gets none, as it
// gets none of GitHub's other variables.
func (x *execution) toolCache() string {
	if x.workflow == nil || x.runner == nil || x.runner.Executor == nil {
		return ""
	}
	switch x.runner.Executor.Name() {
	case "docker":
		return toolCacheVolumePrefix + x.runner.Name
	case "native":
		if x.o.cfg == nil || x.o.cfg.Paths.Dir == "" {
			return ""
		}
		return filepath.Join(x.o.cfg.Paths.Dir, "toolcache", x.runner.Name)
	}
	return ""
}

// toolCacheVolumePrefix names a docker runner's tool cache volume. Not the
// cache volumes' prefix, so §14.4's pruning, which is about caches, leaves it
// alone.
const toolCacheVolumePrefix = "latem-toolcache-"

// repoVars answers the vars context from OpenVault and this repository's
// config (A50, A96), or nil without a config, which leaves vars empty as it
// was before. The vault is read once, when an expression first reads vars,
// and what came of it is kept for [execution.varsFailed].
func (x *execution) repoVars(ctx context.Context) func(environment string) (map[string]string, error) {
	cfg := x.o.config()
	if cfg == nil {
		return nil
	}
	r, ok := cfg.Repos[x.req.Repo]
	if !ok {
		return nil
	}
	if x.vars == nil {
		var vault repovars.Vault
		if cfg.Secrets.Vault {
			vault = x.o.vault
		}
		key := x.req.Repo
		x.vars = &varsReader{read: func() (repovars.Set, error) { return repovars.Read(ctx, vault, key, r) }}
	}
	return x.vars.For
}

// varsFailed is why this job's variables could not be read, if an
// expression read them and they could not be (A96). It is asked once the job
// is compiled, and wins over whatever the compile made of the failure: a job
// that reads a variable never runs with it empty, and a store that did not
// answer is a backend down, retried, not a workflow that is wrong.
func (x *execution) varsFailed(ctx context.Context) error {
	if x.vars == nil {
		return nil
	}
	err := x.vars.failed()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return &secrets.Error{Msg: "this job reads vars, and its variables could not be read", Err: err}
}

// varsReader reads a job's variables once, on first use (A96).
type varsReader struct {
	once sync.Once
	read func() (repovars.Set, error)
	set  repovars.Set
	err  error
	done atomic.Bool
}

func (v *varsReader) get() (repovars.Set, error) {
	v.once.Do(func() {
		v.set, v.err = v.read()
		v.done.Store(true)
	})
	return v.set, v.err
}

// For is the vars context of a job in environment.
func (v *varsReader) For(environment string) (map[string]string, error) {
	s, err := v.get()
	if err != nil {
		return nil, err
	}
	return s.For(environment), nil
}

// plan is the repository's own variables, for a plan.
func (v *varsReader) plan() (map[string]string, error) { return v.For("") }

func (v *varsReader) failed() error {
	if !v.done.Load() {
		return nil
	}
	return v.err
}
