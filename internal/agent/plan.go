package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// maxConcurrentPlans bounds the plans this agent makes at once (A52). A plan
// fetches and reads a repository, and a control plane — confused or hostile —
// sending a burst of them should cost the machine four fetches' worth of work
// at a time, not one per message. Fetches of one repository serialise anyway.
const maxConcurrentPlans = 4

// plan answers a plan command on the transport it came from (A52).
//
// It runs on its own goroutine: a fetch takes seconds, and the transport's
// command stream must keep moving — the cancel for a running build may be the
// next message.
func (a *Agent) plan(ctx context.Context, t transport, req protocol.PlanRequest) {
	select {
	case a.planSlots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.planSlots }()

	res := a.makePlan(ctx, req, speaksRequires(t))
	if res.Error != "" {
		a.log.Warn("plan refused", "plan_id", req.ID, "repo", req.Repo, "code", res.ErrorCode, "err", res.Error)
	} else {
		a.log.Info("planned", "plan_id", req.ID, "repo", req.Repo, "event", req.Event.Name,
			"sha", req.Event.SHA, "workflows", len(res.Workflows))
	}
	if err := t.b.ReportPlan(ctx, res); err != nil {
		a.log.Warn("plan not delivered; the control plane asks again", "plan_id", req.ID, "broker", t.name, "err", err)
	}
}

// makePlan reads the event's commit from this machine's own mirror and plans
// it (INV-5). Nothing here runs anything from the repository. With requires,
// each job also says what it needs of a machine (A61).
func (a *Agent) makePlan(ctx context.Context, req protocol.PlanRequest, requires bool) protocol.PlanResult {
	res := protocol.PlanResult{ID: req.ID}
	fail := func(code protocol.ErrorCode, err error) protocol.PlanResult {
		res.ErrorCode = code
		res.Error = a.redactLine(err.Error())
		return res
	}

	// INV-3: the repo is matched exactly against config and never used to
	// build a path; the key that match produced is what goes further.
	cfg := a.current()
	repoCfg, ok := cfg.Repos[req.Repo]
	if !ok {
		return fail(protocol.ErrUnknownRepo, fmt.Errorf("no repo %q in this machine's config", req.Repo))
	}
	slug, isGitHub := repoCfg.GitHubRepository()
	if !isGitHub {
		slug = req.Repo
	}

	planID := "plan:" + req.ID
	a.noteActive(planID, req.Repo)
	defer a.forgetActive(planID)

	mirror, err := a.src.Fetch(ctx, source.Repo{Key: req.Repo, URL: repoCfg.URL})
	if err != nil {
		return fail(codeOr(source.CodeOf(err), protocol.ErrSource), err)
	}
	fsys, err := mirror.ReadDir(ctx, req.Event.SHA, workflow.Dir, workflow.MaxWorkflowFiles, workflow.MaxWorkflowBytes)
	if err != nil {
		return fail(codeOr(source.CodeOf(err), protocol.ErrSource), err)
	}
	ev := req.Event
	workflows, err := workflow.PlanEvent(fsys, ev, workflow.PlanOptions{
		Repository:   slug,
		LabelAliases: cfg.LabelAliases,
		Vars:         repoCfg.VarsFor(""),
		Changed: func() ([]string, error) {
			// A push compares with what the branch was; anything else — a
			// new branch, a pull request's merge commit — with the commit's
			// first parent.
			from := ""
			if ev.Name == workflow.EventPush {
				from = ev.Before
			}
			return mirror.Changed(ctx, from, ev.SHA)
		},
	})
	if err != nil {
		return fail(codeOr(registry.CodeOf(err), protocol.ErrBadRegistry), err)
	}
	if requires {
		addRequires(ctx, mirror, ev.SHA, workflows)
	}
	for i := range workflows {
		workflows[i].Error = a.redactLine(workflows[i].Error)
		for j := range workflows[i].Jobs {
			workflows[i].Jobs[j].Error = a.redactLine(workflows[i].Jobs[j].Error)
		}
	}
	res.Workflows = workflows
	return res
}

// varDigests is what a heartbeat says of a repository's variables (A68): a
// value's digest, which tells two machines' answers apart and is all that
// comparing them needs. Variables are not secrets, but they are this machine's
// config, and the control plane has no use for the values. Only those outside
// any environment: nothing that decides a job's place reads an environment's.
func varDigests(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for name, value := range vars {
		sum := sha256.Sum256([]byte(value))
		out[name] = hex.EncodeToString(sum[:8])
	}
	return out
}

func codeOr(code, fallback protocol.ErrorCode) protocol.ErrorCode {
	if code != "" {
		return code
	}
	return fallback
}

// redactLine scrubs what the agent always redacts (§16) from a message bound
// for the control plane. A plan's errors quote workflow files and git's own
// words, and git's words can include a remote URL.
func (a *Agent) redactLine(s string) string {
	for _, secret := range a.alwaysRedact() {
		if len(secret) > 0 {
			s = strings.ReplaceAll(s, string(secret), "***")
		}
	}
	return s
}
