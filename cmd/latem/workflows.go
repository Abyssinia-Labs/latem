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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func newWorkflowsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflows",
		Short: "Check a repository's GitHub workflows against this machine",
		Args:  noArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newWorkflowsCheckCommand())
	return cmd
}

type workflowsCheckFlags struct {
	repo, branch string
	fix          bool
}

func newWorkflowsCheckCommand() *cobra.Command {
	var f workflowsCheckFlags
	cmd := &cobra.Command{
		Use:   "check [path]",
		Short: "Say which of a checkout's workflow jobs this machine can run, and the fix for each it cannot",
		Long: `Say which of a checkout's workflow jobs this machine can run, and the fix
for each it cannot.

Reads .github/workflows in a local checkout (default: the current directory)
and plans it as a control plane would ask this machine to — a push to the
default branch, a pull request into it, and a workflow_dispatch of each file
that has one — through the same planner and compiler the agent uses, offline.
Nothing is fetched and nothing runs.

For every job it reports whether a runner here takes it, and if not why:

  - a runs-on label no runner carries, with the [label_aliases] line to add
  - a remote action not in [actions] allowed, or not pinned to a commit
  - secrets with no value in any scope a run would look in (names only)
  - vars with no value, which a job reads as empty
  - what latem refuses outright: a matrix, a reusable workflow, services:,
    container:, and anything else the compiler names

A job's if: is not evaluated: the question is whether this machine could run
the job, not whether this event would.

--fix adds the [label_aliases] and [actions] allowed entries it suggests to
config.toml, and asks the running agent to reload. Pinning an action and
storing a secret stay yours to do.

The checkout is matched to a [repos.*] entry by its origin remote, or by
--repo, for its vars and secrets.

Exit codes:
  0  every job these events plan can run here
  1  at least one cannot, or the checkout could not be read
  2  usage`,
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runWorkflowsCheck(cmd, dir, f)
		},
	}
	cmd.Flags().StringVar(&f.repo, "repo", "", "the [repos.*] entry this checkout is (default: matched by its origin remote)")
	cmd.Flags().StringVar(&f.branch, "branch", "", "the branch pushes and pull requests are checked against (default: the repo's)")
	cmd.Flags().BoolVar(&f.fix, "fix", false, "add the suggested [label_aliases] and [actions] entries to config.toml")
	return cmd
}

// checkTarget is the checkout being checked and what the config says of it.
type checkTarget struct {
	dir     string
	slug    string       // owner/name, or "" when nothing says
	repoKey string       // the [repos.*] key, or ""
	repo    *config.Repo // nil when the checkout is not in the config
	branch  string
}

func runWorkflowsCheck(cmd *cobra.Command, dir string, f workflowsCheckFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(filepath.Join(abs, workflow.Dir)); err != nil || !info.IsDir() {
		return fmt.Errorf("%s has no %s directory; give the path of a checkout", abs, workflow.Dir)
	}
	target, err := resolveCheckTarget(ctx, cfg, abs, f)
	if err != nil {
		return err
	}
	stores, err := newSecretStores(cfg.Paths)
	if err != nil {
		return err
	}

	rep, err := analyzeWorkflows(ctx, cfg, target, stores)
	if err != nil {
		return err
	}
	var fixed []string
	if f.fix && rep.hasFixes() {
		paths := config.DerivePaths(configDir())
		fixed, err = applyWorkflowFixes(paths, cfg, rep)
		if err != nil {
			return err
		}
		if cfg, err = loadConfig(); err != nil {
			return err
		}
		if rep, err = analyzeWorkflows(ctx, cfg, target, stores); err != nil {
			return err
		}
		defer announceConfigChange(out, paths)
	}

	printCheckReport(out, target, rep)
	if len(fixed) > 0 {
		printf(out, "\nfixed in %s:\n", cfg.Paths.File)
		for _, l := range fixed {
			printf(out, "  %s\n", l)
		}
		printf(out, "\n")
	} else if rep.hasFixes() {
		printf(out, "\n`latem workflows check --fix` adds the [label_aliases] and [actions] entries above.\n")
	}
	if rep.failed() > 0 || rep.broken() > 0 {
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%d of %d jobs cannot run on this machine as it is, "+
			"and %d workflow file(s) could not be planned", rep.failed(), rep.jobs(), rep.broken())}
	}
	return nil
}

// resolveCheckTarget finds the [repos.*] entry a checkout is, by --repo or by
// its origin remote.
func resolveCheckTarget(ctx context.Context, cfg *config.Config, dir string, f workflowsCheckFlags) (checkTarget, error) {
	t := checkTarget{dir: dir, branch: f.branch}
	if f.repo != "" {
		key, err := resolveRepoKey(f.repo)
		if err != nil {
			return t, err
		}
		t.repoKey, t.repo = key, cfg.Repos[key]
	} else if origin := originURL(ctx, dir); origin != "" {
		if slug, ok := (config.Repo{URL: origin}).GitHubRepository(); ok {
			t.slug = slug
		}
		for _, k := range slices.Sorted(maps.Keys(cfg.Repos)) {
			r := cfg.Repos[k]
			slug, ok := r.GitHubRepository()
			if r.URL == origin || (ok && t.slug != "" && strings.EqualFold(slug, t.slug)) {
				t.repoKey, t.repo = k, r
				break
			}
		}
	}
	if t.repo != nil {
		if slug, ok := t.repo.GitHubRepository(); ok {
			t.slug = slug
		}
	}
	if t.branch == "" {
		t.branch = "main"
		if t.repo != nil {
			for _, r := range t.repo.AllowedRefs {
				if !strings.ContainsAny(r, "*?[") && !strings.HasPrefix(r, "refs/") {
					t.branch = r
					break
				}
			}
		}
	}
	return t, nil
}

// originURL is the checkout's origin remote, or "". A seam, so a test needs no
// git repository.
var originURL = func(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// #nosec G204 -- argv, no shell; dir is the operator's own path, after -C.
	c := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	c.Env = serviceGitEnv(true)
	b, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// checkSHA stands in for the commit: the planner wants one, and nothing it
// checks depends on which.
const checkSHA = "0000000000000000000000000000000000000c4c"

// finding is one thing wrong with a job or a workflow, and how to fix it.
type finding struct {
	level level
	text  string
	fixes []string
}

type jobReport struct {
	key, name string
	runner    string // the runner that takes it, when one does
	findings  []finding
}

func (j jobReport) level() level {
	worst := levelPass
	for _, f := range j.findings {
		worst = max(worst, f.level)
	}
	return worst
}

type workflowReport struct {
	file, name string
	events     []string
	triggers   []string // declared, for a workflow none of the events planned
	err        string
	jobs       []jobReport
	findings   []finding
}

type checkReport struct {
	workflows []workflowReport
	// aliases and allowed are what --fix would add.
	aliases map[string][]string
	allowed []string
}

func (r checkReport) hasFixes() bool { return len(r.aliases) > 0 || len(r.allowed) > 0 }

func (r checkReport) jobs() int {
	n := 0
	for _, w := range r.workflows {
		n += len(w.jobs)
	}
	return n
}

// failed counts the jobs that cannot run here.
func (r checkReport) failed() int {
	n := 0
	for _, w := range r.workflows {
		for _, j := range w.jobs {
			if j.level() == levelFail {
				n++
			}
		}
	}
	return n
}

// broken counts the workflows that could not be planned at all: a file that
// does not parse, or whose triggers do not.
func (r checkReport) broken() int {
	n := 0
	for _, w := range r.workflows {
		if w.err != "" && len(w.jobs) == 0 {
			n++
		}
	}
	return n
}

// analyzeWorkflows plans and compiles every workflow in the checkout.
func analyzeWorkflows(ctx context.Context, cfg *config.Config, t checkTarget, stores secretStores) (checkReport, error) {
	rep := checkReport{aliases: map[string][]string{}}
	inv, err := secretInventory(ctx, cfg, stores)
	if err != nil {
		return rep, err
	}
	root, err := os.OpenRoot(t.dir)
	if err != nil {
		return rep, err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	files, err := workflowFilesIn(fsys)
	if err != nil {
		return rep, err
	}
	parsed := map[string]*parsedWorkflow{}
	for _, file := range files {
		parsed[file] = parseForCheck(fsys, file)
	}

	opts := workflow.PlanOptions{
		Repository:   t.slug,
		LabelAliases: cfg.LabelAliases,
		Changed:      func() ([]string, error) { return checkoutFiles(fsys), nil },
	}
	if t.repo != nil {
		opts.Vars = t.repo.VarsFor("")
	}

	planned := map[string]*plannedFile{}
	record := func(ev protocol.Event, pws []protocol.PlannedWorkflow) {
		for _, pw := range pws {
			p := planned[pw.File]
			if p == nil {
				p = &plannedFile{pw: pw, ev: ev}
				planned[pw.File] = p
			}
			p.events = append(p.events, eventLabel(ev, t.branch))
		}
	}
	for _, ev := range checkEvents(t) {
		pws, err := workflow.PlanEvent(fsys, ev, opts)
		if err != nil {
			return rep, fmt.Errorf("planning a %s: %w", ev.Name, err)
		}
		record(ev, pws)
	}
	for _, file := range files {
		pw := parsed[file]
		if pw.err != nil || !slices.Contains(pw.triggers, workflow.EventWorkflowDispatch) {
			continue
		}
		ev := dispatchEvent(t, file, pw.dispatchInputs)
		pws, err := workflow.PlanEvent(fsys, ev, opts)
		if err != nil {
			wr := planned[file]
			if wr == nil {
				wr = &plannedFile{pw: protocol.PlannedWorkflow{File: file, Name: file}, ev: ev}
				planned[file] = wr
			}
			wr.dispatchErr = err.Error()
			continue
		}
		record(ev, pws)
	}

	fetcher, cleanup, err := newStubFetcher()
	if err != nil {
		return rep, err
	}
	defer cleanup()

	for _, file := range files {
		wr := workflowReport{file: file, name: file}
		pf := planned[file]
		pw := parsed[file]
		if pw.wf != nil && pw.wf.Name != "" {
			wr.name = pw.wf.Name
		}
		if pf == nil {
			wr.triggers = pw.triggers
			if pw.err != nil {
				wr.err = pw.err.Error()
			}
			rep.workflows = append(rep.workflows, wr)
			continue
		}
		wr.events = pf.events
		wr.err = pf.pw.Error
		if pf.dispatchErr != "" {
			wr.findings = append(wr.findings, finding{level: levelWarn,
				text: "workflow_dispatch: " + pf.dispatchErr})
		}
		envs := map[string]bool{}
		for _, pj := range pf.pw.Jobs {
			jr := checkJob(ctx, cfg, t, inv, fetcher, root, pw, pf, pj, &rep)
			wr.jobs = append(wr.jobs, jr)
			envs[pj.Environment] = true
		}
		wr.findings = append(wr.findings, checkVars(t, pw, envs)...)
		rep.workflows = append(rep.workflows, wr)
	}
	return rep, nil
}

// plannedFile is one workflow as the events planned it.
type plannedFile struct {
	pw          protocol.PlannedWorkflow
	ev          protocol.Event // the first event that planned it
	events      []string
	dispatchErr string
}

func eventLabel(ev protocol.Event, branch string) string {
	switch ev.Name {
	case workflow.EventPush:
		return "push to " + branch
	case workflow.EventPullRequest:
		return "pull_request"
	}
	return ev.Name
}

// checkEvents are the events a control plane most often asks a plan for: a
// push to the default branch and a pull request into it.
func checkEvents(t checkTarget) []protocol.Event {
	payload, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/" + t.branch, "after": checkSHA, "repository": map[string]any{"full_name": t.slug},
	})
	pr, _ := json.Marshal(map[string]any{
		"action": "opened", "number": 1,
		"pull_request": map[string]any{
			"number": 1, "base": map[string]any{"ref": t.branch}, "head": map[string]any{"ref": "latem-check"},
		},
		"repository": map[string]any{"full_name": t.slug},
	})
	return []protocol.Event{
		{Name: workflow.EventPush, Ref: "refs/heads/" + t.branch, SHA: checkSHA, Actor: "latem", Payload: payload},
		{Name: workflow.EventPullRequest, Action: "opened", Ref: "refs/pull/1/merge", SHA: checkSHA,
			BaseRef: t.branch, HeadRef: "latem-check", Actor: "latem", Payload: pr},
	}
}

// dispatchEvent dispatches file with a value for every required input that
// has no default: the first option of a choice, false, 0, or a placeholder.
func dispatchEvent(t checkTarget, file string, decl map[string]dispatchDecl) protocol.Event {
	inputs := map[string]string{}
	for name, in := range decl {
		if !in.Required || in.Default != nil {
			continue
		}
		switch in.Type {
		case "choice":
			if len(in.Options) > 0 {
				inputs[name] = in.Options[0]
			}
		case "boolean":
			inputs[name] = "false"
		case "number":
			inputs[name] = "0"
		default:
			inputs[name] = "latem-check"
		}
	}
	return protocol.Event{Name: workflow.EventWorkflowDispatch, Ref: "refs/heads/" + t.branch, SHA: checkSHA,
		Workflow: file, Inputs: inputs, Actor: "latem", Payload: json.RawMessage(`{}`)}
}

// parsedWorkflow is a workflow file as this command reads it for itself: the
// triggers it declares and its dispatch inputs, which the planner does not
// export.
type parsedWorkflow struct {
	file           string
	data           []byte
	wf             *workflow.Workflow
	err            error
	triggers       []string
	dispatchInputs map[string]dispatchDecl
}

type dispatchDecl struct {
	Required bool            `json:"required"`
	Default  json.RawMessage `json:"default"`
	Type     string          `json:"type"`
	Options  []string        `json:"options"`
}

func parseForCheck(fsys fs.FS, file string) *parsedWorkflow {
	p := &parsedWorkflow{file: file}
	data, err := fs.ReadFile(fsys, file)
	if err != nil {
		p.err = err
		return p
	}
	if len(data) > workflow.MaxWorkflowBytes {
		p.err = fmt.Errorf("%s is larger than %d bytes", file, workflow.MaxWorkflowBytes)
		return p
	}
	p.data = data
	w, err := workflow.Parse(data, file)
	if err != nil {
		p.err = err
		return p
	}
	p.wf = w
	on := w.On
	if len(on) == 0 {
		on = w.OnYAML11
	}
	p.triggers, p.dispatchInputs = readTriggers(on)
	return p
}

// readTriggers reads `on:` in its three spellings: a name, a list, a map.
func readTriggers(raw json.RawMessage) ([]string, map[string]dispatchDecl) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, nil
	}
	var decl map[string]dispatchDecl
	if d, ok := m[workflow.EventWorkflowDispatch]; ok {
		var body struct {
			Inputs map[string]dispatchDecl `json:"inputs"`
		}
		if json.Unmarshal(d, &body) == nil {
			decl = body.Inputs
		}
	}
	return slices.Sorted(maps.Keys(m)), decl
}

// workflowFilesIn lists .github/workflows/*.y(a)ml, as the planner reads them.
func workflowFilesIn(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, workflow.Dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		p := path.Join(workflow.Dir, e.Name())
		if !e.IsDir() && workflow.IsWorkflowPath(p) {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files, nil
}

// maxCheckoutFiles bounds the walk below. A paths filter is matched against
// the files it lists, and a repository larger than this is matched against
// the first of them.
const maxCheckoutFiles = 50000

// checkoutFiles stands in for an event's diff: every file in the checkout,
// so a paths filter matches when any file it names exists. .git and
// node_modules are skipped, as nothing a workflow filters on lives there.
func checkoutFiles(fsys fs.FS) []string {
	var files []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		files = append(files, p)
		if len(files) >= maxCheckoutFiles {
			return fs.SkipAll
		}
		return nil
	})
	return files
}

// checkJob compiles one planned job and asks every question about it.
func checkJob(
	ctx context.Context, cfg *config.Config, t checkTarget, inv inventory, fetcher *stubFetcher,
	root *os.Root, pw *parsedWorkflow, pf *plannedFile, pj protocol.PlannedJob, rep *checkReport,
) jobReport {
	jr := jobReport{key: pj.Key, name: pj.Name}
	fail := func(text string, fixes ...string) {
		jr.findings = append(jr.findings, finding{level: levelFail, text: text, fixes: fixes})
	}
	if pj.Error != "" {
		fail("refused: " + pj.Error)
		return jr
	}

	// A fresh parse, so blanking this job's if: touches no other job's
	// compile.
	w, err := workflow.Parse(pw.data, pw.file)
	if err != nil {
		fail("refused: " + err.Error())
		return jr
	}
	declared := w.Jobs[pj.Key]
	copied := *declared
	copied.If = ""
	w.Jobs[pj.Key] = &copied

	var vars func(string) map[string]string
	if t.repo != nil {
		vars = t.repo.VarsFor
	}
	fetcher.reset()
	job, err := workflow.CompileWith(ctx, w, pj.Key, checkScope(t, pf, pj, w), workflow.Options{
		Actions: rootActions{root: root},
		// Every action is let through and recorded, and the machine's own
		// policy applied to the record below — so one refused action does not
		// hide the rest of the job, or the next refused action.
		Policy:       actions.Policy{Allowed: []string{"*/*"}, AllowUnisolated: cfg.Actions.AllowUnisolated},
		Fetcher:      fetcher,
		LabelAliases: cfg.LabelAliases,
		Vars:         vars,
		WorkflowRun:  true,
	})
	checkActions(cfg, fetcher.seen(), rep, fail)
	if err != nil {
		fail("refused: " + err.Error())
		return jr
	}

	jr.runner = checkRunner(cfg, w, pj.Key, job, rep, fail)
	checkSecrets(t, inv, job, &jr)
	return jr
}

// checkScope is what the job is compiled against: what a run of it would be
// given (internal/run's workflowScope), with every need succeeded.
func checkScope(t checkTarget, pf *plannedFile, pj protocol.PlannedJob, w *workflow.Workflow) expr.Scope {
	ev := pf.ev
	refName, refType := strings.TrimPrefix(ev.Ref, "refs/heads/"), "branch"
	if ev.Name == workflow.EventPullRequest {
		refName = strings.TrimPrefix(ev.Ref, "refs/pull/")
	}
	owner, _, _ := strings.Cut(t.slug, "/")
	payload := map[string]any{}
	_ = json.Unmarshal(ev.Payload, &payload)
	inputs := map[string]any{}
	for k, v := range pf.pw.Inputs {
		inputs[k] = v
	}
	needs := map[string]protocol.NeedResult{}
	for _, n := range pj.Needs {
		needs[n] = protocol.NeedResult{Result: "success"}
	}
	name := w.Name
	if name == "" {
		name = w.Path
	}
	baseRef, headRef := "", ""
	if ev.Name == workflow.EventPullRequest {
		baseRef, headRef = ev.BaseRef, ev.HeadRef
	}
	return expr.Scope{
		Contexts: map[string]expr.Value{
			"github": map[string]any{
				"repository": t.slug, "repository_owner": owner,
				"server_url": workflow.GitHubServerURL, "api_url": workflow.GitHubAPIURL,
				"graphql_url": workflow.GitHubGraphQLURL,
				"ref":         ev.Ref, "ref_name": refName, "ref_type": refType, "sha": ev.SHA,
				"job": pj.Key, "workflow": name, "event_name": ev.Name, "event": payload,
				"actor": ev.Actor, "triggering_actor": ev.Actor, "base_ref": baseRef, "head_ref": headRef,
				"run_id": "1", "run_number": float64(1), "run_attempt": float64(1), "workspace": t.dir,
			},
			"inputs": inputs,
			"env":    map[string]any{},
			"vars":   map[string]any{},
			"needs":  workflow.NeedsContext(needs),
		},
		Status:       expr.StatusSuccess,
		WorkspaceDir: t.dir,
	}
}

// rootActions reads a local action's metadata through the checkout's
// os.Root, as the agent's own loader does inside a workspace.
type rootActions struct{ root *os.Root }

func (l rootActions) Load(dir string) (*workflow.Action, error) {
	clean := path.Clean(strings.TrimPrefix(dir, "./"))
	if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return nil, fmt.Errorf("uses: %s reaches outside the repository", dir)
	}
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := fs.ReadFile(l.root.FS(), path.Join(clean, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return workflow.ParseAction(data, clean)
	}
	return nil, fmt.Errorf("uses: %s — no action.yml or action.yaml there", dir)
}

// stubFetcher answers every remote action with a placeholder JavaScript action
// and records the reference. The check is offline: what an action's own
// action.yml says is the next run's to discover, and whether this machine
// would fetch it at all is the policy's, applied to the record.
type stubFetcher struct {
	dir  string
	refs []actions.Ref
}

func newStubFetcher() (*stubFetcher, func(), error) {
	dir, err := os.MkdirTemp("", "latem-check-")
	if err != nil {
		return nil, nil, err
	}
	stub := "name: placeholder\nruns:\n  using: node20\n  main: index.js\n"
	if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte(stub), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	return &stubFetcher{dir: dir}, func() { _ = os.RemoveAll(dir) }, nil
}

func (s *stubFetcher) Fetch(_ context.Context, r actions.Ref) (actions.Resolved, error) {
	s.refs = append(s.refs, r)
	sha := r.Ref
	if !r.Pinned() {
		sha = strings.Repeat("0", 40)
	}
	return actions.Resolved{Ref: r, SHA: sha, Dir: s.dir, Rel: "placeholder"}, nil
}

func (s *stubFetcher) reset() { s.refs = nil }

func (s *stubFetcher) seen() []actions.Ref { return slices.Clone(s.refs) }

// checkActions applies the machine's [actions] policy to what a job used.
func checkActions(cfg *config.Config, refs []actions.Ref, rep *checkReport, fail func(string, ...string)) {
	allow := actions.Policy{Allowed: cfg.Actions.Allowed}
	pinned := cfg.Actions.PinningRequired()
	done := map[string]bool{}
	for _, r := range refs {
		if done[r.String()] {
			continue
		}
		done[r.String()] = true
		if allow.Permits(r) != nil {
			fail(fmt.Sprintf("uses %s: %s is not in [actions] allowed", r, r.Slug()),
				fmt.Sprintf(`[actions] allowed += %q`, r.Slug()))
			if !slices.Contains(rep.allowed, r.Slug()) {
				rep.allowed = append(rep.allowed, r.Slug())
			}
		}
		if pinned && !r.Pinned() {
			fail(fmt.Sprintf("uses %s: %q is a tag or branch, and this machine runs actions pinned to a commit", r, r.Ref),
				fmt.Sprintf("pin it in the workflow: uses: %s@<the 40-character commit %s names>", strings.TrimSuffix(r.String(), "@"+r.Ref), r.Ref),
				"or accept moving refs on this machine: [actions] require_pinned = false")
		}
	}
}

// checkRunner is gate 13 for a compiled job: the first runner carrying its
// labels, and able to run its kind of steps (internal/run's selectRunner).
func checkRunner(cfg *config.Config, w *workflow.Workflow, key string, job *registry.Job, rep *checkReport,
	fail func(string, ...string)) string {
	needDocker := job.RequiredExecutor() == registry.ExecutorDocker
	var labelsOnly *config.Runner
	for _, r := range cfg.Runners {
		if !hasAllLabels(r.Labels, job.RunsOn) {
			continue
		}
		isolates := r.Executor == config.ExecutorDocker || r.Executor == config.ExecutorTart
		if (!needDocker || r.Executor == config.ExecutorDocker) && (!job.RequiresIsolation || isolates) {
			return r.Name
		}
		if labelsOnly == nil {
			labelsOnly = r
		}
	}
	switch {
	case labelsOnly == nil:
		if !suggestAliases(cfg, w, key, job.RunsOn, rep, fail) {
			fail(fmt.Sprintf("no runner here carries all of %s", strings.Join(job.RunsOn, ", ")))
		}
	case needDocker && labelsOnly.Executor != config.ExecutorDocker:
		fail(fmt.Sprintf("a docker:// step or a Docker action needs a docker runner, and %q is %s", labelsOnly.Name, labelsOnly.Executor))
	default:
		fail(fmt.Sprintf("it uses remote actions, which run only on an isolating runner, and %q is %s",
			labelsOnly.Name, labelsOnly.Executor),
			"[actions] allow_unisolated = true   # runs them as you, with everything you can reach (A47)")
	}
	return ""
}

// suggestAliases names the runs-on labels no runner carries, with the alias
// that maps each onto this machine when the label says which platform it
// means. It reports whether it said anything.
func suggestAliases(cfg *config.Config, w *workflow.Workflow, key string, compiled []string, rep *checkReport,
	fail func(string, ...string)) bool {
	if w == nil || w.Jobs[key] == nil {
		return false
	}
	have := map[string]bool{}
	for _, r := range cfg.Runners {
		for _, l := range r.Labels {
			have[l] = true
		}
	}
	said := false
	for _, raw := range w.Jobs[key].RunsOn {
		l := strings.ToLower(strings.TrimSpace(raw))
		if l == "self-hosted" || have[l] || cfg.LabelAliases[l] != nil || expr.Has(l) {
			continue
		}
		if !slices.Contains(compiled, l) {
			// A hosted label the compiler mapped (ubuntu-latest → linux): the
			// missing label is the platform, which no alias supplies.
			continue
		}
		target := aliasTarget(l, have)
		if target == nil {
			fail(fmt.Sprintf("runs-on %q: no runner here carries it, and nothing says which one should", l),
				fmt.Sprintf(`[label_aliases] %q = [<the labels of the runner that should take it>]`, l))
		} else {
			fail(fmt.Sprintf("runs-on %q: no runner here carries it", l),
				fmt.Sprintf(`[label_aliases] %q = %s`, l, tomlStringList(target)))
			rep.aliases[l] = target
		}
		said = true
	}
	for _, l := range compiled {
		raw := slices.ContainsFunc(w.Jobs[key].RunsOn, func(r string) bool { return strings.EqualFold(strings.TrimSpace(r), l) })
		if !have[l] && !raw {
			fail(fmt.Sprintf("needs a runner labelled %q, and this machine has none — it is a job for another machine", l))
			said = true
		}
	}
	return said
}

// aliasTarget is the label a vendor's runs-on label means on this machine,
// when its name says: an ubuntu or linux label is this machine's linux
// runner, a macos one its macos runner. Anything else is the operator's call.
func aliasTarget(label string, have map[string]bool) []string {
	switch {
	case (strings.Contains(label, "ubuntu") || strings.Contains(label, "linux")) && have["linux"]:
		return []string{"linux"}
	case strings.Contains(label, "macos") && have["macos"]:
		return []string{"macos"}
	}
	return nil
}

// checkSecrets names the secrets the job reads that no store here holds under
// any scope its run would look in.
func checkSecrets(t checkTarget, inv inventory, job *registry.Job, jr *jobReport) {
	var missing []string
	for _, name := range job.Secrets {
		// GITHUB_TOKEN is minted for the job by the control plane (A52).
		if name == workflow.TokenSecret {
			continue
		}
		if !inv.has(name, t.repoKey, job.Environment) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	where := "on this machine"
	if job.Environment != "" {
		where = "for environment " + job.Environment
	}
	f := finding{level: levelFail, text: fmt.Sprintf("secrets with no value %s: %s", where, strings.Join(missing, ", "))}
	scope := ""
	if t.repoKey != "" {
		scope = "--repo " + t.repoKey + " "
		if job.Environment != "" {
			scope += "--env " + job.Environment + " "
		}
	}
	for _, n := range missing {
		f.fixes = append(f.fixes, "latem secrets set "+scope+n)
	}
	jr.findings = append(jr.findings, f)
}

// varRef finds ${{ vars.NAME }} and vars['NAME'] in a workflow's text. A
// regular expression over the file rather than the compiled job: a var a
// condition reads matters as much as one a step does, and either reads as
// empty when unset.
var varRef = regexp.MustCompile(`\bvars\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_]*)|\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\])`)

// checkVars names the vars a workflow reads that nothing here sets.
func checkVars(t checkTarget, pw *parsedWorkflow, envs map[string]bool) []finding {
	seen := map[string]bool{}
	var missing []string
	for _, m := range varRef.FindAllSubmatch(pw.data, -1) {
		name := string(m[1]) + string(m[2])
		if seen[name] {
			continue
		}
		seen[name] = true
		set := false
		if t.repo != nil {
			for env := range envs {
				if _, ok := t.repo.VarsFor(env)[name]; ok {
					set = true
				}
			}
		}
		if !set {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	f := finding{level: levelWarn, text: "vars with no value here, which read as empty: " + strings.Join(missing, ", ")}
	if t.repoKey != "" {
		for _, n := range missing {
			f.fixes = append(f.fixes, "latem vars set --repo "+t.repoKey+" "+n+" <value>")
		}
	}
	return []finding{f}
}

// applyWorkflowFixes writes the suggested aliases and allowlist entries.
func applyWorkflowFixes(paths config.Paths, cfg *config.Config, rep checkReport) ([]string, error) {
	var lines []string
	_, err := editConfig(paths, func(body string) (string, error) {
		var err error
		for _, l := range slices.Sorted(maps.Keys(rep.aliases)) {
			if _, exists := cfg.LabelAliases[l]; exists {
				continue
			}
			literal := tomlStringList(rep.aliases[l])
			if body, err = setTableKey(body, []string{"label_aliases"}, []string{"label_aliases"}, l, literal); err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("[label_aliases] %s = %s", tomlKey(l), literal))
		}
		if len(rep.allowed) > 0 {
			allowed := slices.Clone(cfg.Actions.Allowed)
			for _, a := range rep.allowed {
				if !slices.Contains(allowed, a) {
					allowed = append(allowed, a)
				}
			}
			literal := tomlStringList(allowed)
			if body, err = setTableKey(body, []string{"actions"}, []string{"actions"}, "allowed", literal); err != nil {
				return "", err
			}
			lines = append(lines, "[actions] allowed = "+literal)
		}
		return body, nil
	})
	return lines, err
}

func printCheckReport(w io.Writer, t checkTarget, rep checkReport) {
	st := newStyler(w)
	what := t.dir
	switch {
	case t.repoKey != "":
		what += fmt.Sprintf(" (repos.%s, %s)", t.repoKey, orDash(t.slug))
	case t.slug != "":
		what += fmt.Sprintf(" (%s — not in this machine's [repos.*]; `latem repo add %s` allows it)", t.slug, t.slug)
	default:
		what += " (not matched to a [repos.*] entry; --repo names one, for its vars and secrets)"
	}
	printf(w, "%s\n", st.heading("checking "+what))

	for _, wr := range rep.workflows {
		title := wr.file
		if wr.name != wr.file {
			title += "  " + strconv.Quote(wr.name)
		}
		printf(w, "\n%s\n", st.bold(title))
		switch {
		case len(wr.events) == 0 && wr.err != "":
			printf(w, "  %s %s\n", st.wrap(levelFail.color(), levelFail.marker()), wr.err)
			continue
		case len(wr.events) == 0:
			printf(w, "  %s\n", st.dim(fmt.Sprintf("not planned for a push to %s, a pull request, or a dispatch (on: %s)",
				t.branch, strings.Join(wr.triggers, ", "))))
			continue
		}
		if wr.err != "" {
			// Planned as broken: the planner puts a file it cannot read in
			// the plan, so that it becomes a failed check, not an absence.
			printf(w, "  %s %s\n", st.wrap(levelFail.color(), levelFail.marker()), wr.err)
			continue
		}
		printf(w, "  %s\n", st.dim("planned for: "+strings.Join(wr.events, ", ")))
		for _, f := range wr.findings {
			printFinding(w, st, "", f)
		}
		for _, j := range wr.jobs {
			lv := j.level()
			label := j.key
			if j.name != j.key {
				label += " (" + j.name + ")"
			}
			detail := ""
			if lv != levelFail && j.runner != "" {
				detail = st.dim("  runs on " + j.runner)
			}
			printf(w, "  %s %s%s\n", st.wrap(lv.color(), lv.marker()), label, detail)
			for _, f := range j.findings {
				printFinding(w, st, "       ", f)
			}
		}
	}
	printf(w, "\n%d of %d jobs can run on this machine", rep.jobs()-rep.failed(), rep.jobs())
	if n := rep.broken(); n > 0 {
		printf(w, "; %d workflow file(s) could not be planned", n)
	}
	printf(w, "\n")
}

func printFinding(w io.Writer, st styler, indent string, f finding) {
	marker := "-"
	if f.level == levelWarn {
		marker = st.wrap(levelWarn.color(), "!")
	}
	text := strings.ReplaceAll(strings.TrimRight(f.text, "\n"), "\n", "\n"+indent+"    ")
	printf(w, "%s  %s %s\n", indent, marker, text)
	for _, fix := range f.fixes {
		printf(w, "%s      %s %s\n", indent, st.dim("fix:"), fix)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func hasAllLabels(have, want []string) bool {
	for _, l := range want {
		if !slices.Contains(have, l) {
			return false
		}
	}
	return true
}
