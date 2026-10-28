package run

import (
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow"
)

// GitHub's public endpoints. A job compiled from a workflow gets these
// whatever host its repository is on: they are what github.server_url and the
// API clients a workflow calls default to, and latem has no GitHub Enterprise
// host to substitute (A50).
const (
	githubServerURL  = "https://github.com"
	githubAPIURL     = "https://api.github.com"
	githubGraphQLURL = "https://api.github.com/graphql"
)

// githubRepository is the repository as GitHub names it, owner/name (A50).
//
// The config key is the fallback, which is what github.repository was before
// A50: a repository reached by a local path has no owner/name to read, and
// the key is still more use to a step than nothing. ok reports which one it is.
func (x *execution) githubRepository() (slug string, ok bool) {
	if r, found := x.o.cfg.Repos[x.req.Repo]; found {
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
	}
	// The same number as $LATEM_BUILD_NUMBER, and absent for the same reason
	// when the row predates it: a workflow falling back to its own number
	// cannot do so on a variable that is set.
	if x.run.Number > 0 {
		env["GITHUB_RUN_NUMBER"] = strconv.FormatInt(x.run.Number, 10)
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

// repoVars answers the vars context from this repository's config (A50), or
// nil without one, which leaves vars empty as it was before.
func (x *execution) repoVars() func(environment string) map[string]string {
	if x.o.cfg == nil {
		return nil
	}
	r, ok := x.o.cfg.Repos[x.req.Repo]
	if !ok {
		return nil
	}
	return r.VarsFor
}
