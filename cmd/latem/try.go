package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

type tryFlags struct {
	dir, workflow, event, ref, repo string
	inputs, matrix                  []string
	detach                          bool
}

func newTryCommand() *cobra.Command {
	var f tryFlags
	cmd := &cobra.Command{
		Use:   "try <job>",
		Short: "Run one workflow job from this working tree, uncommitted changes included",
		Long: `Run one workflow job from this working tree, uncommitted changes included,
on this machine, with no push and no control plane (RUNNER_SPEC A76).

The working tree becomes a commit on no ref, as ` + "`git add -A && git commit`" + ` would
make it, without touching your index, branches or stash. Its objects are added
to the agent's mirror, and the job runs pinned to it, as a push to your branch
would run it, on the runners, toolchains and secrets of this machine. The job
is read from the commit, never from the working tree, so an edit made after
this returns is not in the run, and the log names the commit.

<job> is a job's id under jobs:. The workflow file is the one that has it, or
--workflow (checks.yml, or .github/workflows/checks.yml) when more than one
does. The checkout is -C, default the current directory, matched to a
[repos.*] entry by its origin remote or by --repo.

What a try does not ask: on: and the job's if: are not evaluated, because you
named the job you want run. Its needs are taken as succeeded, with no
outputs. A matrix job's leg is picked with --matrix key=value.

The job gets no GitHub token: GITHUB_TOKEN is this machine's secret of that
name, if it has one (` + "`latem secrets set --repo R GITHUB_TOKEN`" + `), and
nothing about the run reaches a control plane.

Before anything is queued the job is checked as ` + "`latem workflows check`" + `
checks it, and a job this machine cannot run is refused with the fix.

Output:
  the run's log on stdout, commentary on stderr; Ctrl-C cancels the run
  --detach   queue it, print the run id, and exit

Exit codes:
  0  the run succeeded, or was queued with --detach
  1  the run did not succeed, or this machine cannot run the job
  2  usage
  3  no agent is listening (without --detach)

Examples:
  latem try test
  latem try -C ~/src/app --workflow checks.yml beacon
  latem try deploy --event workflow_dispatch --input target=staging
  latem try build --matrix os=ubuntu-latest`,
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			job := ""
			if len(args) == 1 {
				job = args[0]
			}
			return runTry(cmd, job, f)
		},
	}
	cmd.Flags().StringVarP(&f.dir, "dir", "C", ".", "the checkout")
	cmd.Flags().StringVar(&f.workflow, "workflow", "", "the workflow file, when more than one has the job")
	cmd.Flags().StringVar(&f.event, "event", workflow.EventPush, "push or workflow_dispatch")
	cmd.Flags().StringArrayVar(&f.inputs, "input", nil, "a workflow_dispatch input as key=value; repeatable")
	cmd.Flags().StringVar(&f.ref, "ref", "", "the branch or tag github.ref names (default: the checkout's branch)")
	cmd.Flags().StringArrayVar(&f.matrix, "matrix", nil, "a matrix value as key=value that picks the leg; repeatable")
	cmd.Flags().StringVar(&f.repo, "repo", "", "the [repos.*] entry the checkout is (default: by its origin remote)")
	cmd.Flags().BoolVar(&f.detach, "detach", false, "queue the run and exit without following it")
	return cmd
}

// treeCommit is a working tree made into a commit (A76).
type treeCommit struct {
	SHA  string // the commit; Base itself when the tree is clean
	Base string // HEAD, its parent
	// Branch is HEAD's branch, or "" for a detached HEAD.
	Branch string
	// Changed is `git diff-tree --name-status` of Base and SHA, a line each.
	Changed []string
}

func runTry(cmd *cobra.Command, job string, f tryFlags) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	if f.event != workflow.EventPush && f.event != workflow.EventWorkflowDispatch {
		return usageErrorf("--event %q: a try is a push or a workflow_dispatch (A76)", f.event)
	}
	inputs, err := parsePairs("--input", f.inputs)
	if err != nil {
		return err
	}
	if len(inputs) > 0 && f.event != workflow.EventWorkflowDispatch {
		return usageErrorf("--input is a workflow_dispatch's; add --event workflow_dispatch")
	}
	matrix, err := parsePairs("--matrix", f.matrix)
	if err != nil {
		return err
	}

	st, closeState, err := openState(ctx)
	if err != nil {
		return err
	}
	defer closeState()
	cfg := st.cfg

	top, err := tryTopLevel(ctx, f.dir)
	if err != nil {
		return err
	}
	target, err := resolveCheckTarget(ctx, cfg, top, workflowsCheckFlags{repo: f.repo})
	if err != nil {
		return err
	}
	if target.repo == nil {
		what := "its origin remote names no [repos.*] entry here"
		if target.slug != "" {
			what = fmt.Sprintf("%s is not in this machine's [repos.*]; `latem repo add %s` allows it", target.slug, target.slug)
		}
		return fmt.Errorf("%s cannot be tried here: %s, or --repo names one", top, what)
	}
	target.workspace = top

	root, err := os.OpenRoot(top)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	file, err := tryWorkflowFile(fsys, f.workflow, job)
	if err != nil {
		return err
	}

	tc, err := tryCommit(ctx, top)
	if err != nil {
		return err
	}
	ref, refNote, err := tryRef(f.ref, tc.Branch, target.branch)
	if err != nil {
		return err
	}

	actor := currentActor()
	ev := tryEvent(target.slug, f.event, file, ref, tc, inputs, actor)
	opts := workflow.PlanOptions{Repository: target.slug, LabelAliases: cfg.LabelAliases, Vars: target.repo.VarsFor("")}
	pw, err := workflow.PlanFile(fsys, file, ev, opts)
	if err != nil {
		return fmt.Errorf("planning %s: %w", file, err)
	}
	pj, err := tryLeg(pw, job, matrix)
	if err != nil {
		return err
	}
	runner, err := precheckTry(ctx, errOut, cfg, target, fsys, file, pw, ev, pj)
	if err != nil {
		return err
	}

	if !f.detach {
		if err := st.requireAgent(ctx); err != nil {
			return err
		}
	}
	if err := addTryCommit(ctx, cfg, target.repoKey, top, tc.SHA); err != nil {
		return err
	}

	id, err := agent.NewRunID()
	if err != nil {
		return err
	}
	req := tryRequest(id, target.repoKey, ref, actor, file, tc.SHA, ev, pw, pj)
	if err := protocol.NewRunCommand(req).Validate(); err != nil {
		return err
	}
	if err := queueTry(ctx, st, req); err != nil {
		return err
	}

	printTrySummary(errOut, file, pj, ev.Name, ref, refNote, runner, tc, id)
	if f.detach {
		printf(cmd.OutOrStdout(), "%s\n", id)
		return nil
	}
	err = watchRun(cmd, st, id, true)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return cancelTry(st, id, actor, errOut)
	}
	return err
}

// queueTry writes the run's row and rings the agent (A76).
//
// The row is written here, not by the agent, because two things on it are
// the point and no run command can carry either: the approval of exactly this
// commit, which running latem try is, and the Local mark that tells the gates
// the ref is a name. The row is created unqueued; Submit's enqueue then queues
// it, from the agent's side of the socket when one is listening and from this
// one when none is, so nothing can claim it before it is complete.
func queueTry(ctx context.Context, st *state, req protocol.RunRequest) error {
	now := time.Now()
	w := store.WorkflowRunOf(req)
	w.Local = true
	row := store.Run{
		ID: req.ID, Repo: req.Repo, Job: req.Job, Ref: req.Ref, State: protocol.StateQueued,
		OriginKind: req.Origin.Kind, OriginActor: req.Origin.Actor, Inputs: req.Inputs, Workflow: w,
		ApprovedAt: &now, ApprovedSHA: req.SHA,
	}
	if err := st.db.Create(ctx, row); err != nil {
		return err
	}
	expires := now.Add(st.queueTTL())
	if _, err := st.client.Submit(ctx, req, local.SubmitOptions{ExpiresAt: expires}); err != nil {
		// The agent took the connection and then failed to answer. The row is
		// here either way, so it is queued from this side and the pool's own
		// poll finds it.
		if qerr := st.db.Enqueue(ctx, req.ID, 0); qerr != nil {
			return errors.Join(err, qerr)
		}
		return st.db.SetExpiry(ctx, req.ID, expires)
	}
	return nil
}

// cancelTry stops the run a Ctrl-C left behind: the run is the terminal's,
// and a try nobody is watching any more is a build nobody wants.
func cancelTry(st *state, id, actor string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.client.Cancel(ctx, protocol.CancelRequest{ID: id, Actor: actor, Reason: "interrupted at the terminal (latem try)"}); err != nil {
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("interrupted, and the run could not be cancelled: %w; `latem cancel %s`", err, id)}
	}
	printf(w, "\ncancelled %s\n", id)
	return &ExitError{Code: ExitFailure, Err: errors.New("interrupted")}
}

// tryRequest is the run a try queues: a workflow run's job (A52) of one, with
// its own id as the workflow run's, numbered by nobody, with no token.
func tryRequest(
	id, repo, ref, actor, file, sha string, ev protocol.Event, pw protocol.PlannedWorkflow, pj protocol.PlannedJob,
) protocol.RunRequest {
	needs := map[string]protocol.NeedResult{}
	for _, n := range pj.Needs {
		needs[n] = protocol.NeedResult{Result: protocol.NeedSuccess}
	}
	if len(needs) == 0 {
		needs = nil
	}
	var inputs map[string]string
	if ev.Name == workflow.EventWorkflowDispatch && len(pw.Inputs) > 0 {
		inputs = pw.Inputs
	}
	return protocol.RunRequest{
		ID: id, Repo: repo, Job: pj.Key, Ref: ref, Inputs: inputs,
		Origin:   protocol.Origin{Kind: "cli", Actor: actor, RequestedAt: time.Now()},
		Workflow: file, SHA: sha, Event: &ev, Needs: needs,
		WorkflowRunID: id, RunAttempt: 1, Matrix: pj.Matrix,
	}
}

// tryEvent is the event a try runs as. Its payload holds what latem knows and
// nothing it would have to invent (A76): no head_commit, commits or pusher.
func tryEvent(slug, name, file, ref string, tc treeCommit, inputs map[string]string, actor string) protocol.Event {
	payload := map[string]any{"ref": ref}
	if owner, repo, ok := strings.Cut(slug, "/"); ok {
		payload["repository"] = map[string]any{"full_name": slug, "name": repo, "owner": map[string]any{"login": owner}}
	}
	ev := protocol.Event{Name: name, Ref: ref, SHA: tc.SHA, Actor: actor}
	if name == workflow.EventWorkflowDispatch {
		ev.Workflow, ev.Inputs = file, inputs
	} else {
		payload["after"] = tc.SHA
		if tc.SHA != tc.Base {
			// The try is one commit pushed onto HEAD.
			payload["before"], ev.Before = tc.Base, tc.Base
		}
	}
	ev.Payload, _ = json.Marshal(payload)
	return ev
}

// tryRef is github.ref: --ref, or the checkout's branch. A detached HEAD runs
// as a push to the repository's default branch, and note says so.
func tryRef(flag, branch, defaultBranch string) (ref, note string, err error) {
	name := flag
	switch {
	case name != "":
	case branch != "":
		name = branch
	default:
		name, note = defaultBranch, fmt.Sprintf("HEAD is detached, so this runs as a push to %s (--ref to change it)", defaultBranch)
	}
	ref = name
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + name
	}
	if err := registry.ValidateRef(ref); err != nil {
		return "", "", usageErrorf("--ref %q: %v", name, err)
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return "", "", usageErrorf("--ref %q is neither a branch nor a tag", name)
	}
	return ref, note, nil
}

// tryWorkflowFile finds the workflow that has job: --workflow, matched by path,
// file name or name without extension, or the one file declaring it.
func tryWorkflowFile(fsys fs.FS, flag, job string) (string, error) {
	files, err := workflowFilesIn(fsys)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(files) == 0) {
		return "", fmt.Errorf("this checkout has no %s; latem try runs a workflow's job", workflow.Dir)
	}
	if err != nil {
		return "", err
	}
	if flag != "" {
		var hits []string
		for _, f := range files {
			base := path.Base(f)
			if f == flag || base == flag || strings.TrimSuffix(base, path.Ext(base)) == flag {
				hits = append(hits, f)
			}
		}
		if len(hits) != 1 {
			return "", usageErrorf("--workflow %q names %d of this checkout's workflows (%s)", flag, len(hits), strings.Join(files, ", "))
		}
		files = hits
	}

	byFile := map[string][]string{}
	var has []string
	for _, f := range files {
		p := parseForCheck(fsys, f)
		if p.wf == nil {
			continue
		}
		byFile[f] = p.wf.JobNames()
		if p.wf.Jobs[job] != nil {
			has = append(has, f)
		}
	}
	switch {
	case job != "" && len(has) == 1:
		return has[0], nil
	case len(has) > 1:
		return "", usageErrorf("job %q is in %s; --workflow picks one", job, strings.Join(has, " and "))
	}
	var lines []string
	for _, f := range slices.Sorted(maps.Keys(byFile)) {
		lines = append(lines, fmt.Sprintf("  %s: %s", f, strings.Join(byFile[f], ", ")))
	}
	if job == "" {
		return "", usageErrorf("name a job to try; this checkout has:\n%s", strings.Join(lines, "\n"))
	}
	return "", usageErrorf("no workflow here has a job %q; this checkout has:\n%s", job, strings.Join(lines, "\n"))
}

// tryLeg is the planned job to run: the only one with that key, or the matrix
// leg every --matrix value matches.
func tryLeg(pw protocol.PlannedWorkflow, job string, want map[string]string) (protocol.PlannedJob, error) {
	var legs []protocol.PlannedJob
	for _, pj := range pw.Jobs {
		if pj.Key == job {
			legs = append(legs, pj)
		}
	}
	if len(legs) == 0 {
		return protocol.PlannedJob{}, fmt.Errorf("%s planned no job %q", pw.File, job)
	}
	if len(legs) == 1 && len(want) == 0 {
		return legs[0], nil
	}
	values := func(pj protocol.PlannedJob) map[string]string {
		raw := map[string]json.RawMessage{}
		_ = json.Unmarshal(pj.Matrix, &raw)
		out := map[string]string{}
		for k, v := range raw {
			var s string
			if json.Unmarshal(v, &s) != nil {
				s = string(v)
			}
			out[k] = s
		}
		return out
	}
	var picked []protocol.PlannedJob
	for _, pj := range legs {
		vals := values(pj)
		ok := true
		for k, v := range want {
			if got, has := vals[k]; !has || got != v {
				ok = false
				break
			}
		}
		if ok {
			picked = append(picked, pj)
		}
	}
	if len(picked) == 1 {
		return picked[0], nil
	}
	var lines []string
	for _, pj := range legs {
		vals := values(pj)
		var flags []string
		for _, k := range slices.Sorted(maps.Keys(vals)) {
			flags = append(flags, fmt.Sprintf("--matrix %s=%s", k, vals[k]))
		}
		lines = append(lines, "  "+strings.Join(flags, " "))
	}
	if len(legs) == 1 {
		return protocol.PlannedJob{}, usageErrorf("job %q has no matrix leg with those values; it is:\n%s", job, strings.Join(lines, "\n"))
	}
	return protocol.PlannedJob{}, usageErrorf("job %q has %d matrix legs and %d match; pick one:\n%s",
		job, len(legs), len(picked), strings.Join(lines, "\n"))
}

// precheckTry checks the job as `latem workflows check` would, through the
// same planner and compiler, before anything is queued: a job no runner here
// takes, an action [actions] refuses, or a job the compiler refuses is
// reported with its fix, and nothing is queued (A76). It returns the runner
// that takes the job.
func precheckTry(
	ctx context.Context, w io.Writer, cfg *config.Config, t checkTarget, fsys fs.FS,
	file string, pw protocol.PlannedWorkflow, ev protocol.Event, pj protocol.PlannedJob,
) (string, error) {
	stores, err := newSecretStores(cfg.Paths)
	if err != nil {
		return "", err
	}
	inv, err := secretInventory(ctx, cfg, stores)
	if err != nil {
		return "", err
	}
	fetcher, cleanup, err := newStubFetcher()
	if err != nil {
		return "", err
	}
	defer cleanup()
	parsed := parseForCheck(fsys, file)
	if parsed.err != nil {
		return "", parsed.err
	}
	rep := checkReport{aliases: map[string][]string{}}
	jr := checkJob(ctx, cfg, t, inv, fetcher, fsys, parsed, &plannedFile{pw: pw, ev: ev, all: []protocol.Event{ev}}, pj, &rep)
	st := newStyler(w)
	for _, f := range jr.findings {
		printFinding(w, st, "", f)
	}
	if jr.level() == levelFail {
		msg := fmt.Sprintf("this machine cannot run %s as it is; nothing was queued", pj.Key)
		if rep.hasFixes() {
			msg += "\n`latem workflows check --fix` adds the [label_aliases] and [actions] entries above"
		}
		return "", &ExitError{Code: ExitFailure, Err: errors.New(msg)}
	}
	return jr.runner, nil
}

func printTrySummary(w io.Writer, file string, pj protocol.PlannedJob, event, ref, refNote, runner string, tc treeCommit, id string) {
	st := newStyler(w)
	what := pj.Name
	if what != pj.Key {
		what = pj.Key + " (" + pj.Name + ")"
	}
	printf(w, "%s\n", st.heading(fmt.Sprintf("try %s › %s at %.12s, as a %s on %s", path.Base(file), what, tc.SHA, event, ref)))
	if refNote != "" {
		printf(w, "  %s\n", refNote)
	}
	if n := len(tc.Changed); tc.SHA == tc.Base {
		printf(w, "  %s\n", st.dim("the tree is clean: this is HEAD itself"))
	} else {
		shown := tc.Changed[:min(n, 8)]
		more := ""
		if n > len(shown) {
			more = fmt.Sprintf(", and %d more", n-len(shown))
		}
		printf(w, "  %s\n", st.dim(fmt.Sprintf("%d path(s) differ from HEAD %.12s: %s%s", n, tc.Base, strings.Join(shown, ", "), more)))
	}
	if runner != "" {
		printf(w, "  %s\n", st.dim("runs on "+runner))
	}
	printf(w, "  %s\n\n", st.dim(fmt.Sprintf("run %s — `latem logs %s`, Ctrl-C cancels", id, id)))
}

// tryTopLevel is the checkout's root. A seam, so a test needs no git.
var tryTopLevel = func(ctx context.Context, dir string) (string, error) {
	out, err := tryGit(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not a git checkout: %w", dir, err)
	}
	return out, nil
}

// tryCommit makes the working tree a commit. A seam; [commitWorkingTree] is
// what it is.
var tryCommit = commitWorkingTree

// addTryCommit puts the commit into the agent's mirror (A76). A seam: no test
// writes a mirror or clones a remote.
var addTryCommit = func(ctx context.Context, cfg *config.Config, key, top, sha string) error {
	mgr, err := source.New(source.Config{MirrorDir: cfg.Paths.Mirrors, WorkDir: cfg.Paths.Work})
	if err != nil {
		return err
	}
	_, err = mgr.AddCommit(ctx, source.Repo{Key: key, URL: cfg.Repos[key].URL}, top, sha)
	return err
}

// commitWorkingTree makes the commit `git add -A && git commit` would, without
// touching the checkout's index, refs or stash (A76): `git add -A` into a copy
// of the index, `write-tree`, and `commit-tree` onto HEAD. A tree equal to
// HEAD's is HEAD, and no commit is made.
//
// Not `git stash create`, which records tracked changes only and so leaves out
// a new workflow file, the commonest change to CI there is.
func commitWorkingTree(ctx context.Context, top string) (treeCommit, error) {
	base, err := tryGit(ctx, top, nil, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return treeCommit{}, fmt.Errorf("%s has no commit to try a tree on: %w", top, err)
	}
	tc := treeCommit{Base: base}
	tc.Branch, _ = tryGit(ctx, top, nil, "symbolic-ref", "--quiet", "--short", "HEAD")

	tmp, err := os.MkdirTemp("", "latem-try-")
	if err != nil {
		return treeCommit{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	index := filepath.Join(tmp, "index")
	env := []string{"GIT_INDEX_FILE=" + index}

	// A copy of the real index keeps its stat cache, so add -A hashes only
	// what changed; a checkout with no index starts from HEAD's tree.
	real, err := tryGit(ctx, top, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return treeCommit{}, err
	}
	if !filepath.IsAbs(real) {
		real = filepath.Join(top, real)
	}
	// #nosec G304 G703 -- the checkout's own index, as its git names it; the
	// copy goes to a directory this function made.
	if b, err := os.ReadFile(real); err == nil {
		if err := os.WriteFile(index, b, 0o600); err != nil { // #nosec G703 -- see above
			return treeCommit{}, err
		}
	} else if _, err := tryGit(ctx, top, env, "read-tree", base); err != nil {
		return treeCommit{}, err
	}
	if _, err := tryGit(ctx, top, env, "add", "-A"); err != nil {
		return treeCommit{}, err
	}
	tree, err := tryGit(ctx, top, env, "write-tree")
	if err != nil {
		return treeCommit{}, err
	}
	baseTree, err := tryGit(ctx, top, nil, "rev-parse", "--verify", "--end-of-options", base+"^{tree}")
	if err != nil {
		return treeCommit{}, err
	}
	if tree == baseTree {
		tc.SHA = base
		return tc, nil
	}
	on := tc.Branch
	if on == "" {
		on = "a detached HEAD"
	}
	if tc.SHA, err = tryGit(ctx, top, nil, "commit-tree", "--no-gpg-sign", "-p", base,
		"-m", "latem try: the working tree on "+on, tree); err != nil {
		return treeCommit{}, err
	}
	changed, err := tryGit(ctx, top, nil, "diff-tree", "-r", "--no-renames", "--no-commit-id", "--name-status", base, tc.SHA)
	if err != nil {
		return treeCommit{}, err
	}
	for _, l := range strings.Split(changed, "\n") {
		if status, name, ok := strings.Cut(l, "\t"); ok {
			tc.Changed = append(tc.Changed, status+" "+name)
		}
	}
	return tc, nil
}

// tryGit runs git in the operator's checkout, with their git config — it is
// their repository — and none of the git state variables a shell may have
// set, which would point it somewhere else.
func tryGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// #nosec G204 -- argv, no shell; dir is the operator's own checkout, and
	// every revision is one git itself printed.
	c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	c.Env = append(serviceGitEnv(true), env...)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
