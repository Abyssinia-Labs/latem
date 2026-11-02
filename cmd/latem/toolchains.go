package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
)

// The toolchains step of `latem setup` (A60): what the jobs this machine
// takes need that it lacks, and — asked one at a time, on a terminal — the
// installs a command can do.
//
// Each install is an argv, run as it is (INV-3), with stdin closed and in a
// session of its own, so nothing it starts can open the terminal to ask for a
// password: what needs sudo, a licence or the App Store is the operator's,
// and is said rather than done. Homebrew runs without updating itself or
// upgrading what depends on what it installs, so adding one tool cannot move
// a JDK a working build relies on.

// toolchainDeps are the step's reaches outside the process, each a seam: the
// machine running the tests must not have things installed on it.
type toolchainDeps struct {
	// probe finds what the machine has, as a step whose PATH is stepPath
	// would.
	probe func(ctx context.Context, stepPath string) system.Toolchains
	// image reports whether Docker has ref locally.
	image func(ctx context.Context, ref string) bool
	// install runs one fix, its output to w.
	install  func(ctx context.Context, argv []string, w io.Writer) error
	lookPath func(name string) (string, error)
	// brewOwner names who owns the Homebrew that brew, a path, belongs to,
	// and whether this account can install into it.
	brewOwner func(brew string) (owner string, mine bool)
	// host is the machine's operating system.
	host func() system.Host
	// stores are where this machine's secrets are, read for their names.
	stores func(config.Paths) (secretStores, error)
}

func realToolchainDeps() toolchainDeps {
	sys := system.New(system.Options{})
	return toolchainDeps{
		probe: sys.Toolchains,
		image: func(ctx context.Context, ref string) bool {
			_, err := sys.Exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", "--", ref)
			return err == nil
		},
		install: runInstall,
		lookPath: func(name string) (string, error) {
			// A fix that is latem's own runs this binary, which need be on
			// no PATH: a machine set up by hand has it wherever it was put.
			if name == "latem" {
				return os.Executable()
			}
			return lookPathOr(name, servicePath(sys))
		},
		brewOwner: brewOwner,
		host:      sys.Host,
		stores:    func(p config.Paths) (secretStores, error) { return newSecretStores(p) },
	}
}

// lookPathOr finds name on this shell's PATH or else on also, the PATH the
// agent's steps get: an account made for the agent may have Homebrew on
// launchd's PATH and not in its login shell's, which is not missing it.
func lookPathOr(name, also string) (string, error) {
	p, err := exec.LookPath(name)
	if err == nil {
		return p, nil
	}
	for _, dir := range filepath.SplitList(also) {
		c := filepath.Join(dir, name)
		// #nosec G703 -- dir is from servicePath's fixed list and name is a
		// fix's program (brew, docker, sdkmanager…), never a repository's.
		if fi, serr := os.Stat(c); serr == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", err
}

// brewOwner reads who owns the Homebrew prefix brew is in, and whether this
// account may write its Cellar. One Homebrew serves a Mac, installed by one
// user; a second account — a build user made for the agent — can run what it
// installed and cannot install anything into it.
func brewOwner(brew string) (string, bool) {
	prefix := filepath.Dir(filepath.Dir(brew))
	dir := filepath.Join(prefix, "Cellar")
	if _, err := os.Stat(dir); err != nil {
		dir = prefix
	}
	mine := syscall.Access(dir, 2 /* W_OK */) == nil
	owner := "another user"
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			owner = strconv.FormatUint(uint64(st.Uid), 10)
			if u, err := user.LookupId(owner); err == nil {
				owner = u.Username
			}
		}
	}
	return owner, mine
}

// runInstall runs one install. Setsid takes it off the terminal: sudo,
// asked for a password, fails instead of prompting.
func runInstall(ctx context.Context, argv []string, w io.Writer) error {
	// #nosec G204 -- argv, no shell; every argv is one of needs.Gaps' fixes
	// or a docker pull of a configured image.
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1",
		"HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1")
	c.Stdin = nil
	c.Stdout, c.Stderr = w, w
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c.Run()
}

// jobNeed is one job, the runner here that takes it, and what it needs.
type jobNeed struct {
	ref    string // "take-one apple.yml apple", or "iOS apps"
	runner string
	report needs.Report
}

// want is something jobs here need that the machine lacks.
type want struct {
	gap    needs.Gap
	runner *config.Runner
	jobs   []string
}

func (w want) id() string { return w.runner.Name + "\x00" + w.gap.Missing }

// buildKind is a kind of build setup asks a machine with no repository
// about, and what it needs of the machine.
type buildKind struct {
	name, label string
	mac         bool // only a Mac builds it
	requires    []needs.Requirement
}

var buildKinds = []buildKind{
	{"ios", "iOS apps", true, []needs.Requirement{{Key: needs.Xcode}, {Key: needs.IOSSimulator}}},
	{"macos", "macOS apps", true, []needs.Requirement{{Key: needs.Xcode}}},
	{"android", "Android apps", false, []needs.Requirement{{Key: needs.JDK, Version: "17"}, {Key: needs.AndroidSDK}}},
	{"linux", "Linux builds", false, nil},
}

// setupToolchains is the step. It reports whether it changed config.toml.
func setupToolchains(cmd *cobra.Command, p *prompter, deps toolchainDeps) (bool, error) {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	cfg, err := loadConfig()
	if err != nil {
		return false, err
	}
	asking := interactive(cmd)

	var jobs []jobNeed
	if len(cfg.Repos) == 0 {
		if jobs, err = askBuildKinds(p, out, cfg, asking); err != nil {
			return false, err
		}
	} else {
		jobs = repoJobNeeds(ctx, out, cfg)
	}

	sys := system.New(system.Options{})
	probed := map[string]system.Toolchains{}
	tcFor := func(r *config.Runner) system.Toolchains {
		if r.Executor != config.ExecutorNative {
			return system.Toolchains{Tools: map[string]bool{}}
		}
		p := ghcmd.PrependPath(r.NativePathPrepend(), servicePath(sys))
		tc, ok := probed[p]
		if !ok {
			tc = deps.probe(ctx, p)
			probed[p] = tc
		}
		return tc
	}
	images := imageWants(ctx, cfg, deps)

	changed := false
	asked := map[string]bool{}
	for {
		var next *want
		for _, w := range append(images, missingFor(cfg, jobs, tcFor)...) {
			if !asked[w.id()] {
				next = &w
				break
			}
		}
		if next == nil {
			break
		}
		asked[next.id()] = true
		printWant(out, *next)
		installed := false
		for _, argv := range next.gap.Fix {
			ran, err := offerFix(ctx, out, p, asking, deps, argv)
			if err != nil {
				return changed, err
			}
			if !ran {
				continue
			}
			installed = true
			wrote, err := configureInstall(out, cfg, next.runner, argv, deps)
			if err != nil {
				printf(out, "    %v\n", err)
			}
			if wrote {
				changed = true
				if cfg, err = loadConfig(); err != nil {
					return changed, err
				}
			}
		}
		for _, m := range append(next.gap.YourLines(), next.gap.Manual...) {
			printf(out, "    by hand: %s\n", m)
		}
		if installed {
			clear(probed)
		}
	}

	switch left := len(imageWants(ctx, cfg, deps)) + len(missingFor(cfg, jobs, tcFor)); {
	case len(asked) == 0 && len(jobs) == 0 && len(cfg.Repos) > 0:
		printf(out, "  no job in %s goes to a runner here, so nothing to check; "+
			"`latem workflows check --repo <key>` says why\n", strings.Join(sortedStrings(cfg.Repos), ", "))
	case len(asked) == 0 && len(jobs) > 0:
		printf(out, "  nothing the jobs here need is missing\n")
	case len(asked) == 0:
	case left == 0:
		printf(out, "\n  nothing the jobs here need is missing now\n")
	default:
		printf(out, "\n  %d still missing; `latem doctor` lists %s\n", left, map[bool]string{true: "it", false: "them"}[left == 1])
	}
	m, err := openMachineView(ctx, cfg, deps, fromMirrorThenRemote)
	if err != nil {
		return changed, err
	}
	defer m.close()
	if plan, err := m.plan(ctx); err == nil {
		printPlan(out, plan)
	} else {
		printf(out, "  %v\n", err)
	}
	return changed, nil
}

// repoJobNeeds reads every configured repository, from the agent's mirror or
// else the remote, and returns what each job a runner here takes needs.
func repoJobNeeds(ctx context.Context, out io.Writer, cfg *config.Config) []jobNeed {
	empty, err := os.MkdirTemp("", "latem-setup-workspace-")
	if err != nil {
		printf(out, "  %v\n", err)
		return nil
	}
	defer func() { _ = os.RemoveAll(empty) }()

	var jobs []jobNeed
	for _, key := range sortedStrings(cfg.Repos) {
		files, err := readRepo(ctx, cfg, key, fromMirrorThenRemote)
		if err != nil {
			printf(out, "  %s was not read: %v\n", key, err)
			continue
		}
		target := repoCheckTarget(cfg, key)
		target.fsys, target.where, target.commit, target.workspace = files.fsys, files.where, files.sha, empty
		rep, err := analyzeWorkflows(ctx, cfg, target, secretStores{})
		files.close()
		if err != nil {
			printf(out, "  %s's workflows were not read: %v\n", key, err)
			continue
		}
		// A job a policy refuses here still comes here once that is settled
		// (setup's repositories step says how), so its needs count.
		for _, wr := range rep.workflows {
			for _, j := range wr.jobs {
				if j.needs != nil && j.carrier != "" && !j.stopped {
					jobs = append(jobs, jobNeed{ref: fmt.Sprintf("%s %s %s", key, path.Base(wr.file), j.name),
						runner: j.carrier, report: *j.needs})
				}
			}
		}
	}
	return jobs
}

// askBuildKinds asks a machine with no repository what it will build, and
// gives each kind to the runner here that would take it.
func askBuildKinds(p *prompter, out io.Writer, cfg *config.Config, asking bool) ([]jobNeed, error) {
	if !asking {
		printf(out, "  no repositories, so nothing to check against\n")
		return nil, nil
	}
	names := make([]string, len(buildKinds))
	for i, k := range buildKinds {
		names[i] = k.name
	}
	answer, err := p.ask("  what will this machine build? " + strings.Join(names, ", ") +
		" — any of them, or Enter to skip: ")
	if err != nil || answer == "" {
		return nil, err
	}
	var jobs []jobNeed
	for _, word := range strings.FieldsFunc(strings.ToLower(answer), func(r rune) bool { return r == ',' || r == ' ' }) {
		i := slices.IndexFunc(buildKinds, func(k buildKind) bool { return k.name == word })
		if i < 0 {
			printf(out, "  %q is not one of %s\n", word, strings.Join(names, ", "))
			continue
		}
		k := buildKinds[i]
		r := runnerFor(cfg, k.mac, len(k.requires) > 0)
		if r == nil {
			printf(out, "  no runner here takes %s\n", k.label)
			continue
		}
		platform := "Linux"
		if slices.Contains(r.Labels, "macos") {
			platform = "macOS"
		}
		jobs = append(jobs, jobNeed{ref: k.label, runner: r.Name,
			report: needs.Report{Platform: platform, Requires: k.requires}})
	}
	return jobs, nil
}

// runnerFor is the runner a kind of build would go to: a native one where
// the host's toolchain is what it needs, on a Mac where only a Mac will do.
func runnerFor(cfg *config.Config, mac, native bool) *config.Runner {
	for _, r := range cfg.Runners {
		switch {
		case mac && !slices.Contains(r.Labels, "macos"):
		case native && r.Executor != config.ExecutorNative:
		case !native && !mac && !slices.Contains(r.Labels, "linux"):
		default:
			return r
		}
	}
	return nil
}

// missingFor is what jobs need that their runners lack, one entry per thing
// and runner, in the order the jobs first need it.
func missingFor(cfg *config.Config, jobs []jobNeed, tcFor func(*config.Runner) system.Toolchains) []want {
	var out []want
	at := map[string]int{}
	for _, j := range jobs {
		r, ok := cfg.Runner(j.runner)
		if !ok {
			continue
		}
		for _, g := range needs.Gaps(j.report, tcFor(r), string(r.Executor)) {
			w := want{gap: g, runner: r}
			i, seen := at[w.id()]
			if !seen {
				i = len(out)
				at[w.id()] = i
				out = append(out, w)
			}
			out[i].jobs = append(out[i].jobs, j.ref)
		}
	}
	return out
}

// imageWants are the docker runners' images Docker does not have yet: a
// first job would otherwise wait on the pull.
func imageWants(ctx context.Context, cfg *config.Config, deps toolchainDeps) []want {
	if _, err := deps.lookPath("docker"); err != nil {
		return nil
	}
	var out []want
	for _, r := range cfg.Runners {
		if r.Executor != config.ExecutorDocker || r.Docker == nil || r.Docker.DefaultImage == "" {
			continue
		}
		img := r.Docker.DefaultImage
		if deps.image(ctx, img) {
			continue
		}
		out = append(out, want{runner: r, jobs: []string{"the " + r.Name + " runner's jobs"},
			gap: needs.Gap{Missing: "its image, " + img, Fix: [][]string{{"docker", "pull", "--", img}}}})
	}
	return out
}

func printWant(out io.Writer, w want) {
	line := "  " + w.gap.Missing
	if w.gap.Have != "" {
		line += " (this machine has " + w.gap.Have + ")"
	}
	jobs := slices.Compact(slices.Clone(w.jobs))
	more := ""
	if len(jobs) > 2 {
		more = fmt.Sprintf(", and %d more", len(jobs)-2)
		jobs = jobs[:2]
	}
	printf(out, "\n%s — for %s%s\n", line, strings.Join(jobs, ", "), more)
}

// offerFix shows one fix and, asked, runs it. It reports whether it ran and
// succeeded; a failure is said, and is not the step's.
func offerFix(ctx context.Context, out io.Writer, p *prompter, asking bool, deps toolchainDeps, argv []string) (bool, error) {
	line := needs.CommandLine(argv)
	bin, err := deps.lookPath(argv[0])
	if err != nil {
		printf(out, "    %s — needs %s, which is not installed\n", line, argv[0])
		return false, nil
	}
	if argv[0] == "brew" {
		if owner, mine := deps.brewOwner(bin); !mine {
			printf(out, "    %s — Homebrew here belongs to %s, and this account cannot install into it; "+
				"%s has to run it\n", line, owner, owner)
			return false, nil
		}
	}
	if !asking {
		printf(out, "    fix: %s\n", line)
		return false, nil
	}
	run, err := p.confirm("    run `"+line+"`?", false)
	if err != nil || !run {
		return false, err
	}
	// The program as found, which may be on the agent's PATH and not this
	// shell's.
	if err := deps.install(ctx, append([]string{bin}, argv[1:]...), out); err != nil {
		printf(out, "    %s failed: %v\n", line, err)
		return false, nil
	}
	return true, nil
}

// kegConfig is where a keg-only Homebrew install puts what a step needs: on
// no PATH the service has, and found by a build only through JAVA_HOME or
// ANDROID_HOME (A60).
func kegConfig(argv []string, prefix, goos string) (prepend []string, env map[string]string) {
	if len(argv) < 3 || argv[0] != "brew" || argv[1] != "install" {
		return nil, nil
	}
	formula := argv[len(argv)-1]
	switch {
	case strings.HasPrefix(formula, "openjdk@"):
		opt := filepath.Join(prefix, "opt", formula)
		home := filepath.Join(opt, "libexec")
		if goos == "darwin" {
			home = filepath.Join(home, "openjdk.jdk", "Contents", "Home")
		}
		return []string{filepath.Join(opt, "bin")}, map[string]string{"JAVA_HOME": home}
	case formula == "ruby":
		return []string{filepath.Join(prefix, "opt", "ruby", "bin")}, nil
	case formula == "android-commandlinetools":
		return nil, map[string]string{"ANDROID_HOME": filepath.Join(prefix, "share", "android-commandlinetools")}
	}
	return nil, nil
}

// configureInstall writes, for a native runner, the path_prepend and env a
// keg-only install needs. A name the runner's env already sets is left as
// it is, and said.
func configureInstall(out io.Writer, cfg *config.Config, r *config.Runner, argv []string, deps toolchainDeps) (bool, error) {
	if r.Executor != config.ExecutorNative || argv[0] != "brew" {
		return false, nil
	}
	brew, err := deps.lookPath("brew")
	if err != nil {
		return false, nil
	}
	prepend, env := kegConfig(argv, filepath.Dir(filepath.Dir(brew)), runtime.GOOS)
	if len(prepend) == 0 && len(env) == 0 {
		return false, nil
	}
	list := slices.Clone(r.NativePathPrepend())
	for _, dir := range prepend {
		if !slices.Contains(list, dir) {
			list = append(list, dir)
		}
	}
	var lines []string
	_, err = editConfig(cfg.Paths, func(body string) (string, error) {
		var err error
		if len(list) > len(r.NativePathPrepend()) {
			if body, err = setRunnerKey(body, cfg, r.Name, []string{"native"}, "path_prepend", tomlStringList(list)); err != nil {
				return "", err
			}
			lines = append(lines, "path_prepend = "+tomlStringList(list))
		}
		for _, k := range sortedStrings(env) {
			switch have := r.NativeEnv()[k]; have {
			case env[k]:
			case "":
				if body, err = setRunnerKey(body, cfg, r.Name, []string{"native", "env"}, k, tomlString(env[k])); err != nil {
					return "", err
				}
				lines = append(lines, k+" = "+tomlString(env[k]))
			default:
				printf(out, "    %s is %s for the %s runner already; left as it is\n", k, have, r.Name)
			}
		}
		return body, nil
	})
	if err != nil || len(lines) == 0 {
		return false, err
	}
	printf(out, "    the %s runner's steps find it now: in config.toml, [runner.native]\n", r.Name)
	for _, l := range lines {
		printf(out, "      %s\n", l)
	}
	return true, nil
}
