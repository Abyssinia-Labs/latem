package run

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// pullRequestRefs is the namespace GitHub keeps a pull request's commits under:
// refs/pull/N/head and refs/pull/N/merge.
const pullRequestRefs = "refs/pull/"

// isPullRequest reports whether this run builds a pull request's commit.
//
// It reads the ref this machine normalized, not the event. A branch's or a
// tag's run builds only a commit on that ref ([execution.pin] for a workflow
// run's job, gate 9 for any other), so a pull request's commit runs under a
// refs/pull/ ref or not at all, whatever the control plane says the event was
// and whatever the pull request says about itself. Fork or not is the
// payload's to say, and the payload is the control plane's, so it is not
// asked.
func (x *execution) isPullRequest() bool {
	return strings.HasPrefix(x.refNormalized, pullRequestRefs)
}

// approvedHere reports whether an operator approved, on this machine, the
// commit this run is about to build.
func (x *execution) approvedHere() bool {
	return registry.ApprovalFor(x.run.ApprovedSHA, x.sha) == registry.ApprovedForThisCommit
}

// machineSecrets are the declared secrets this run would look up on the
// machine. A workflow job's own GitHub token is not one of them: it arrives
// with the run, minted for this job (A52). A called workflow's secret that no
// caller passed is looked up as nothing (A82), so it is not one either.
func (x *execution) machineSecrets() []string {
	var out []string
	for _, name := range x.resolved.Secrets {
		if x.workflow != nil {
			if name == workflow.TokenSecret && x.req.Token != "" {
				continue
			}
			if len(x.secretLookup()(name)) == 0 {
				continue
			}
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// holdPullRequest is the first half of gate 15 (A85): a pull request's run
// that would reach anything of the machine's waits for its commit to be
// approved here, with `latem approve`, as a ref outside the policy does.
//
// Two things reach the machine. A declared secret, which gate 15 is about to
// resolve from every store this machine has. And a runner that does not
// isolate the job (native): its steps run as the agent's own user, where
// secrets.env, the Keychain and the OpenVault socket are all in reach, so
// withholding the declared secrets would withhold nothing (§13.0). A pull
// request's run that needs neither builds as before, and its secrets were
// never asked for.
//
// The pull request cannot change any of this. What it asks for is read from
// its own commit, which is why it is not trusted: a job that names
// `environment: production` is held like any other, and the approval message
// says which environment the approver would be giving it. The approval is a
// local action bound to one commit (INV-5): a broker cannot grant it, and a
// push to the pull request is a new commit, asked about again.
func (x *execution) holdPullRequest() (registry.Decision, bool) {
	if !x.isPullRequest() {
		return registry.Decision{}, true
	}
	if x.approvedHere() {
		x.note("a pull request's commit, approved on this machine at %s: it gets what it asks for", shortSHA(x.sha))
		return registry.Decision{}, true
	}
	wants := x.machineSecrets()
	host := !executorIsolates(x.runner)
	if len(wants) == 0 && !host {
		return registry.Decision{}, true
	}

	var asks []string
	if len(wants) > 0 {
		what := "secrets " + strings.Join(wants, ", ")
		if env := x.resolved.Job.Environment; env != "" {
			what += " of environment " + env
		}
		asks = append(asks, "asks for "+what)
	}
	if host {
		asks = append(asks, fmt.Sprintf("would run on runner %q as this machine's own user, "+
			"where every secret store is in reach", x.runner.Name))
	}
	return registry.Decision{
		State: protocol.StateAwaitingApproval,
		Msg: fmt.Sprintf("a pull request's job %s; read its commit, then approve it with `latem approve %s`",
			strings.Join(asks, ", and "), x.run.ID),
	}, false
}
