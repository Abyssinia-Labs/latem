package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// machineView is this machine, read for readiness (A69): each repository's
// files, held open so that looking again after an install or an edit does not
// read them again, and the probes, which are done afresh every time.
type machineView struct {
	deps      toolchainDeps
	sys       *system.System
	files     map[string]*repoFiles
	unread    []string
	workspace string
}

// openMachineView reads every configured repository from where from says. One
// that cannot be read is said, and the rest still count.
func openMachineView(ctx context.Context, cfg *config.Config, deps toolchainDeps, from treeSource) (*machineView, error) {
	workspace, err := os.MkdirTemp("", "latem-setup-workspace-")
	if err != nil {
		return nil, err
	}
	m := &machineView{deps: deps, sys: system.New(system.Options{}), files: map[string]*repoFiles{}, workspace: workspace}
	for _, key := range sortedStrings(cfg.Repos) {
		files, err := readRepo(ctx, cfg, key, from)
		if err != nil {
			m.unread = append(m.unread, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		m.files[key] = files
	}
	return m, nil
}

func (m *machineView) close() {
	for _, f := range m.files {
		f.close()
	}
	_ = os.RemoveAll(m.workspace)
}

// plan is what is true of the machine now, as config.toml says now: setup
// edits it between looks.
func (m *machineView) plan(ctx context.Context) (readiness.Plan, error) {
	cfg, err := loadConfig()
	if err != nil {
		return readiness.Plan{}, err
	}
	return m.planFor(ctx, cfg)
}

func (m *machineView) planFor(ctx context.Context, cfg *config.Config) (readiness.Plan, error) {
	stores, err := m.deps.stores(cfg.Paths)
	if err != nil {
		return readiness.Plan{}, err
	}
	in := readiness.Input{Machine: cfg.Machine.Name, Host: m.deps.host(), Unread: slices.Clone(m.unread)}
	in.Toolchains = m.deps.probe(ctx, servicePath(m.sys))
	in.Toolchains.Host = in.Host
	probed := map[string]system.Toolchains{}
	for _, r := range cfg.Runners {
		rr := readiness.Runner{Name: r.Name, Executor: string(r.Executor), Labels: r.Labels, Env: r.NativeEnv()}
		if r.Executor == config.ExecutorNative {
			p := ghcmd.PrependPath(r.NativePathPrepend(), servicePath(m.sys))
			tc, ok := probed[p]
			if !ok {
				tc = m.deps.probe(ctx, p)
				tc.Host = in.Host
				probed[p] = tc
			}
			rr.Toolchains = tc
		}
		in.Runners = append(in.Runners, rr)
	}
	for _, w := range imageWants(ctx, cfg, m.deps) {
		in.Images = append(in.Images, readiness.Image{Runner: w.runner.Name, Ref: strings.TrimPrefix(w.gap.Missing, "its image, ")})
	}
	for _, key := range sortedStrings(cfg.Repos) {
		files, ok := m.files[key]
		if !ok {
			continue
		}
		target := repoCheckTarget(cfg, key)
		target.fsys, target.where, target.commit, target.workspace = files.fsys, files.where, files.sha, m.workspace
		rep, err := analyzeWorkflows(ctx, cfg, target, stores)
		if err != nil {
			in.Unread = append(in.Unread, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		for _, wr := range rep.workflows {
			for _, j := range wr.jobs {
				if j.needs == nil {
					continue
				}
				in.Jobs = append(in.Jobs, readiness.Job{
					Ref: fmt.Sprintf("%s %s %s", key, path.Base(wr.file), j.name), Repo: key,
					Labels: j.labels, Report: *j.needs, Runner: j.carrier, Unisolated: j.unisolated,
					Moving: j.moving, NotAllowed: j.notAllowed, Secrets: j.secrets,
				})
			}
		}
	}
	return readiness.Build(in), nil
}

// runSetupPlan is `latem setup --json`: the plan, and nothing touched.
func runSetupPlan(cmd *cobra.Command, deps setupDeps) error {
	if _, err := os.Stat(config.DerivePaths(configDir()).File); err != nil {
		return usageErrorf("this machine has no config yet; run `latem setup` first")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	m, err := openMachineView(ctx, cfg, deps.toolchains, fromMirrorThenRemote)
	if err != nil {
		return err
	}
	defer m.close()
	plan, err := m.plan(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(plan)
}

// machineReadiness is the plan as the agent's heartbeat says it (A70). It reads
// only the agent's own mirrors, which the agent fetches when it has something
// to fetch for: a machine's first look finds none, and says so as unread. An
// explanation that panics on a repository's files must not take the machine's
// builds down with it, so a panic here is an answer it could not give.
func machineReadiness(ctx context.Context, cfg *config.Config, deps toolchainDeps) (out *protocol.Readiness, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	m, err := openMachineView(ctx, cfg, deps, fromMirror)
	if err != nil {
		return nil, err
	}
	defer m.close()
	plan, err := m.planFor(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return readiness.Advert(plan), nil
}

// printPlan is the plan for a log, a script, and the scrollback a closed
// screen leaves: one line per kind, and what stands in the way of the rest.
func printPlan(out io.Writer, plan readiness.Plan) {
	printf(out, "\n  what this machine builds\n")
	for _, k := range plan.Kinds {
		line := fmt.Sprintf("    %-18s %s", k.Title, stateText(k))
		if n := k.Blocking(); n > 0 {
			var titles []string
			for _, it := range k.Items {
				if !it.Soft {
					titles = append(titles, it.Title)
				}
			}
			line += ": " + joinSome(titles, 3)
		}
		printf(out, "%s\n", line)
		for _, n := range k.Notes {
			printf(out, "      %s\n", n)
		}
	}
	for _, u := range plan.Unread {
		printf(out, "    not read: %s\n", u)
	}
	if len(plan.Unclassified) > 0 {
		printf(out, "    %d job%s not classified: %s\n", len(plan.Unclassified), plural(len(plan.Unclassified)), joinSome(plan.Unclassified, 3))
	}
}

// canDrawScreen reports whether setup can take over the terminal. A seam: no
// test has one, and TERM=dumb is how a terminal says it cannot be drawn on.
var canDrawScreen = func(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	out, ok2 := cmd.OutOrStdout().(*os.File)
	term := os.Getenv("TERM")
	return ok && ok2 && isTerminal(in) && isTerminal(out) && term != "" && term != "dumb"
}

// runSetupScreen is the toolchains step on a terminal. It reports whether
// config.toml changed, for the service step.
func runSetupScreen(cmd *cobra.Command, deps toolchainDeps) (bool, error) {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	cfg, err := loadConfig()
	if err != nil {
		return false, err
	}
	printf(out, "  reading this machine's repositories…\n")
	m, err := openMachineView(ctx, cfg, deps, fromMirrorThenRemote)
	if err != nil {
		return false, err
	}
	defer m.close()
	plan, err := m.plan(ctx)
	if err != nil {
		return false, err
	}
	in, isIn := cmd.InOrStdin().(*os.File)
	stdout, isOut := cmd.OutOrStdout().(*os.File)
	if !isIn || !isOut {
		printPlan(out, plan)
		return false, nil
	}
	t := newTerminal(in, stdout)
	changed := false
	screen := newSetupScreen(plan, screenHooks{
		refresh: func() (readiness.Plan, error) { return m.plan(ctx) },
		run: func(k readiness.Kind, it readiness.Item) error {
			return t.suspend(func() error {
				wrote, err := runItem(ctx, stdout, it, k.Runner, deps)
				changed = changed || wrote
				if err != nil {
					printf(stdout, "\n%v\n", err)
				}
				waitForEnter(in, stdout)
				return err
			})
		},
		apply: func(changes []readiness.Change) error {
			if err := applyChanges(changes); err != nil {
				return err
			}
			changed = true
			return nil
		},
		shell: func(title string, lines []string) error {
			return t.suspend(func() error { return openShell(ctx, in, stdout, title, lines) })
		},
		prove: func(k readiness.Kind) error { return proveKind(ctx, m.stepPath(k.Runner), k) },
	}, newStyler(stdout))

	if err := t.enter(); err != nil {
		printPlan(out, plan)
		return false, nil //nolint:nilerr // a terminal stty cannot drive gets the plan, as a log does
	}
	defer t.leave()
	for !screen.done {
		if err := t.draw(screen.view(t.width, t.height)); err != nil {
			return changed, err
		}
		k, ok, err := t.next(ctx)
		if err != nil {
			return changed, err
		}
		if ok {
			screen.key(k)
		}
	}
	t.leave()
	printPlan(out, screen.plan)
	return changed, nil
}

// stepPath is the PATH a step on the named runner gets.
func (m *machineView) stepPath(runner string) string {
	cfg, err := loadConfig()
	if err != nil {
		return servicePath(m.sys)
	}
	if r, ok := cfg.Runner(runner); ok {
		return ghcmd.PrependPath(r.NativePathPrepend(), servicePath(m.sys))
	}
	return servicePath(m.sys)
}

// runItem runs an item's commands in order, stopping at one that fails, and
// writes what a keg-only install needs. It reports whether config.toml
// changed.
func runItem(ctx context.Context, w io.Writer, it readiness.Item, runner string, deps toolchainDeps) (bool, error) {
	changed := false
	for _, argv := range it.Run {
		printf(w, "\n$ %s\n", commandLines([][]string{argv})[0])
		bin, err := deps.lookPath(argv[0])
		if err != nil {
			return changed, fmt.Errorf("it needs %s, which is not installed", argv[0])
		}
		if argv[0] == "brew" {
			if owner, mine := deps.brewOwner(bin); !mine {
				return changed, fmt.Errorf("this Homebrew belongs to %s, and this account cannot install into it", owner)
			}
		}
		if err := deps.install(ctx, append([]string{bin}, argv[1:]...), w); err != nil {
			return changed, err
		}
		cfg, err := loadConfig()
		if err != nil {
			return changed, err
		}
		if r, ok := cfg.Runner(runner); ok {
			wrote, err := configureInstall(w, cfg, r, argv, deps)
			if err != nil {
				return changed, err
			}
			changed = changed || wrote
		}
	}
	return changed, nil
}

// proveKind runs a kind's proof with what a step would have: the service's
// PATH, the runner's env, and nothing from the shell setup was started in. A
// tool that works only from a person's profile fails here, as in a job.
func proveKind(ctx context.Context, stepPath string, k readiness.Kind) error {
	for _, p := range k.Proof {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		c := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...) //nolint:gosec // readiness's fixed proofs, argv and no shell
		c.Env = []string{"PATH=" + stepPath, "HOME=" + os.Getenv("HOME"), "LANG=en_US.UTF-8"}
		for k, v := range p.Env {
			c.Env = append(c.Env, k+"="+v)
		}
		out, err := c.CombinedOutput()
		cancel()
		if err != nil {
			first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			return errors.Join(fmt.Errorf("%s: %w", p.Title, err), textError(first))
		}
	}
	return nil
}

func textError(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// applyChanges writes changes to config.toml, one edit at a time so each
// reads the file as the one before left it, and tells a running agent. An
// edit that would make the config unloadable is refused by editConfig.
func applyChanges(changes []readiness.Change) error {
	paths := config.DerivePaths(configDir())
	for _, c := range changes {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if _, err := editConfig(paths, func(body string) (string, error) { return applyChange(body, cfg, c) }); err != nil {
			return err
		}
	}
	return nil
}

func applyChange(body string, cfg *config.Config, c readiness.Change) (string, error) {
	actions := []string{"actions"}
	switch c.What {
	case readiness.ChangeEnv:
		return setRunnerKey(body, cfg, c.Runner, []string{"native", "env"}, c.Key, tomlString(c.Value))
	case readiness.ChangeLabelAdd, readiness.ChangeLabelDrop:
		r, ok := cfg.Runner(c.Runner)
		if !ok {
			return "", fmt.Errorf("config.toml has no runner %q", c.Runner)
		}
		labels := slices.DeleteFunc(slices.Clone(r.Labels), func(l string) bool { return strings.EqualFold(l, c.Value) })
		if c.What == readiness.ChangeLabelAdd {
			labels = append(labels, c.Value)
		}
		if len(labels) == 0 {
			// A runner must have one, and a native runner with none of a
			// kind's labels is a runner nothing is sent to yet.
			labels = []string{readiness.LabelNative}
		}
		return setRunnerLabels(body, cfg, c.Runner, labels)
	case readiness.ChangeUnisolated:
		return setTableKey(body, actions, actions, "allow_unisolated", "true")
	case readiness.ChangeMovingRefs:
		return setTableKey(body, actions, actions, "require_pinned", "false")
	case readiness.ChangeAllowed:
		allowed := slices.Clone(cfg.Actions.Allowed)
		if !slices.Contains(allowed, c.Value) {
			allowed = append(allowed, c.Value)
		}
		return setTableKey(body, actions, actions, "allowed", tomlStringList(allowed))
	case readiness.ChangeNewRunner:
		return insertAfter(body, []string{"runner"}, fmt.Sprintf(`[[runner]]
name        = %s
executor    = "native"
labels      = [%s]
concurrency = 1

  [runner.native]
  min_free_gb = 60
`, tomlString(c.Value), tomlString(readiness.LabelNative))), nil
	}
	return "", fmt.Errorf("setup does not know how to change %q", c.What)
}
