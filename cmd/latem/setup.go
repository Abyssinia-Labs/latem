package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/broker/remote"
	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

type setupFlags struct {
	url, name, image string
	like             string
	repos            []string
	noRegister       bool
	noService        bool
	noDocker         bool
	noBrowser        bool
	json, plain      bool
}

func newSetupCommand() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Make this machine a latem runner: config, repositories, toolchains, registration, service, doctor",
		Long: `Make this machine a latem runner: config, repositories, toolchains,
registration, service, doctor.

One command for a fresh machine, and safe to run again on one that is half
done — each step looks at what is there and does only what is missing:

  1. config        writes ~/.latem/config.toml when there is none: a native
                   macos runner on a Mac, and a linux docker runner wherever a
                   Docker daemon answers (on Linux, always). With
                   --like <machine>, it then copies that machine's
                   [actions] allowed, and at a terminal, once you say yes,
                   its [label_aliases] and its allow_unisolated and
                   require_pinned (A102). <machine> is a host ssh reaches,
                   or its config.toml as a file.
  2. repositories  adds the repositories --repo names — or, on a terminal and
                   with none configured, asks which — and checks each one's
                   workflows, read without a checkout: one line each, how
                   many of its jobs can run here and what is in the way,
                   with ` + "`latem workflows check`" + ` for the detail. Asked, it
                   offers the label aliases and allowed actions they need
                   (A57).
  3. toolchains    shows what this machine builds, kind by kind, whichever
                   machine its repositories' jobs ask for today, and what is
                   in the way of each (A69). On a terminal this is a screen:
                   it finds what is installed, sets the paths a step needs,
                   runs the installs a command can do once you say yes, opens
                   a shell for the ones that are yours, and turns a kind on
                   with a label once a check of it passes. It never runs
                   sudo: what needs a password, a licence or the App Store is
                   said, for you to do. Off a terminal it prints the plan and
                   goes on; --plain asks one question at a time instead.
  4. register      approves this machine in the browser (` + "`latem register`" + `),
                   unless it is already registered or --no-register keeps it
                   local.
  5. service       installs the agent as a user service and starts it, or
                   restarts it when an earlier step changed what it runs.
  6. doctor        checks the result and names the fix for anything wrong.

Registering before the service starts, rather than after, is deliberate: the
control plane connection is read only when the agent starts, so the other
order would start the agent twice.

--json prints what the screen shows and touches nothing: every kind, what is
in the way of it, and what would close each thing. ` + "`latem setup apply <id>`" + `
closes one of those things as the screen would, for an agent or a script with
no screen (A94).

Then:
  latem repo add <owner/name>         allow another repository
  latem workflows check --repo <key>  what this machine can run of it
  latem secrets set NAME              what its workflows read

Exit codes:
  0  set up, and doctor found nothing failing
  1  a step failed, or doctor did
  2  usage`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := normalizeAgentURL(f.url)
			if err != nil {
				return err
			}
			for i, r := range f.repos {
				slug, err := parseGitHubSlug(r)
				if err != nil {
					return usageErrorf("--repo %s: %v", r, err)
				}
				f.repos[i] = slug
			}
			f.url = url
			if f.json {
				if f.like != "" {
					return usageErrorf("--like writes to config.toml, and --json changes nothing; run them apart")
				}
				return runSetupPlan(cmd, newSetupDeps())
			}
			return runSetup(cmd, f, newSetupDeps())
		},
	}
	cmd.AddCommand(newSetupAndroidSDKCommand(), newSetupApplyCommand())
	cmd.Flags().StringVar(&f.url, "url", registerURL, "control plane agent endpoint to register with")
	cmd.Flags().StringVar(&f.name, "name", "", "machine name (default: this host's name)")
	cmd.Flags().StringSliceVar(&f.repos, "repo", nil, "a GitHub repository this machine builds, as owner/name (repeatable)")
	cmd.Flags().StringVar(&f.like, "like", "", "copy another machine's [actions] allowed, and at a terminal its [label_aliases] and the two settings: a host ssh reaches, or its config.toml")
	cmd.Flags().StringVar(&f.image, "image", "", "default_image for the docker runner (default: latem-ubuntu, pinned by digest)")
	cmd.Flags().BoolVar(&f.noRegister, "no-register", false, "do not register with a control plane; this machine runs local builds only")
	cmd.Flags().BoolVar(&f.noService, "no-service", false, "do not install or restart the agent service")
	cmd.Flags().BoolVar(&f.noDocker, "no-docker", false, "on a Mac, do not add a docker runner even if Docker answers")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the approval link without opening a browser")
	cmd.Flags().BoolVar(&f.json, "json", false, "print what this machine builds and what is in the way, and change nothing")
	cmd.Flags().BoolVar(&f.plain, "plain", false, "ask one question at a time instead of opening a screen")
	return cmd
}

// setupDeps are the parts of setup that reach outside this process: the
// Docker daemon, the service manager, the control plane and doctor. Every one
// is a seam, because the machine running the tests may be a build machine
// whose own agent must not be reinstalled, restarted or re-registered.
type setupDeps struct {
	// findDocker is the daemon a docker runner would use, and the socket to
	// write as [docker] host when it is an engine's own (A111).
	findDocker func(ctx context.Context) (host string, ok bool)
	service    setupService
	register   func(cmd *cobra.Command, f registerFlags) error
	doctor     func(cmd *cobra.Command) error
	toolchains toolchainDeps
	// measure is the machine, for the docker runner's concurrency (A63),
	// with Docker asked at dockerHost (A111).
	measure func(ctx context.Context, dockerHost string) capacity.Machine
	// goos is the platform the config step writes for: a Mac's native
	// runner beside the docker one, or Linux's docker runner alone. Empty is
	// this one, so that a test can say which machine it is.
	goos string
}

type setupService interface {
	Status(ctx context.Context) (system.ServiceState, error)
	Install(ctx context.Context, w io.Writer) error
	Restart(ctx context.Context, w io.Writer) error
}

var newSetupDeps = func() setupDeps {
	return setupDeps{
		findDocker: findDocker,
		service:    systemService{sys: system.New(system.Options{})},
		register:   runRegister,
		doctor:     runDoctorFor,
		toolchains: realToolchainDeps(),
		measure: func(ctx context.Context, dockerHost string) capacity.Machine {
			return measureMachine(ctx, system.New(system.Options{}), dockerHost)
		},
	}
}

func runSetup(cmd *cobra.Command, f setupFlags, deps setupDeps) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	st := newStyler(out)
	step := func(title string) { printf(out, "\n%s\n", st.heading(title)) }
	paths := config.DerivePaths(configDir())
	changed := false
	// One reader of stdin for every question, so that a question does not
	// lose what was typed ahead for the next.
	p := newPrompter(cmd)

	// The machine to be like is read before anything is written, so one
	// that cannot be reached leaves this one as it was (A102).
	var like likeMachine
	if f.like != "" {
		var err error
		if like, err = readLikeMachine(ctx, f.like); err != nil {
			return err
		}
	}
	var liked likeOutcome

	// 1 — config.
	step("1/6 config")
	if _, err := os.Stat(paths.File); os.IsNotExist(err) {
		goos := deps.goos
		if goos == "" {
			goos = runtime.GOOS
		}
		docker := goos != "darwin"
		var dockerHost string
		if docker || !f.noDocker {
			// An engine at a socket of its own, Colima's or OrbStack's, is
			// written down as [docker] host rather than left to a
			// DOCKER_HOST the service would not keep (A111).
			var found bool
			dockerHost, found = deps.findDocker(ctx)
			docker = docker || found
		}
		slots, sized := 0, ""
		if docker {
			// The docker runner takes as many jobs at once as Docker has room
			// for (A63), not a fixed two: Docker Desktop's VM on an 8 GB Mac
			// holds one.
			if m := deps.measure(ctx, dockerHost); m.CPUs > 0 && m.MemoryGB > 0 {
				a := capacity.Recommend(m, []*config.Runner{{Name: "linux", Executor: config.ExecutorDocker}})[0]
				slots = a.Recommended
				sized = fmt.Sprintf("  linux runner: concurrency %d — %s\n", slots, a.Why)
			}
		}
		if err := writeSetupConfig(paths, f, goos, docker, dockerHost, slots); err != nil {
			return err
		}
		changed = true
		printf(out, "  wrote %s\n", paths.File)
		if goos == "darwin" {
			printf(out, "  runners: macos (native)")
			if docker {
				printf(out, ", linux (docker)")
			} else if !f.noDocker {
				printf(out, " — no Docker daemon answered, so no linux runner; start Docker, then `latem setup apply %s`", readiness.ItemNoLinuxRunner)
			}
			printf(out, "\n")
		} else {
			printf(out, "  runners: linux (docker)\n")
		}
		if dockerHost != "" {
			printf(out, "  docker: [docker] host = %s, where %s answers\n", tomlString(dockerHost), dockerEngine(dockerHost))
		}
		printf(out, "%s", sized)
	} else if err != nil {
		return err
	} else {
		printf(out, "  %s exists; left as it is\n", paths.File)
	}
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("%w\n\nsetup stopped here: the agent would not start on this config. "+
			"`latem doctor` names every fix; run `latem setup` again after", err)
	}
	if f.like != "" {
		// Before the repositories, so that each is checked against what
		// this machine now allows.
		if liked, err = setupLike(cmd, p, like, cfg); err != nil {
			return err
		}
		changed = changed || liked.wrote
		if cfg, err = loadConfig(); err != nil {
			return err
		}
	}

	// 2 — repositories.
	step("2/6 repositories")
	if err := setupRepositories(cmd, p, f, cfg); err != nil {
		return err
	}

	// 3 — toolchains.
	step("3/6 toolchains")
	var wrote bool
	if !f.plain && canDrawScreen(cmd) {
		wrote, err = runSetupScreen(cmd, deps.toolchains)
	} else {
		wrote, err = setupToolchains(cmd, p, deps.toolchains)
	}
	if err != nil {
		return err
	}
	changed = changed || wrote
	if cfg, err = loadConfig(); err != nil {
		return err
	}

	// 4 — register.
	step("4/6 register")
	switch {
	case f.noRegister:
		printf(out, "  skipped (--no-register): this machine builds what `latem run` asks, and nothing else\n")
	case registered(cfg):
		printf(out, "  registered with %s as %s\n", cfg.Broker.Remote.URL, orDash(cfg.Machine.ID))
	default:
		if err := deps.register(cmd, registerFlags{url: f.url, name: f.name, noBrowser: f.noBrowser, inSetup: true}); err != nil {
			return err
		}
		changed = true
	}

	// 5 — service.
	step("5/6 service")
	switch {
	case f.noService:
		printf(out, "  skipped (--no-service); run the agent yourself with `latem agent`\n")
	case runtime.GOOS != "darwin" && runtime.GOOS != "linux":
		printf(out, "  no service integration on %s; run `latem agent` under your own supervisor\n", runtime.GOOS)
	default:
		state, err := deps.service.Status(ctx)
		if err != nil {
			return err
		}
		switch {
		case !state.Installed:
			if err := deps.service.Install(ctx, out); err != nil {
				return err
			}
		case state.Outdated():
			// An older latem's definition: rewritten, which restarts the agent.
			printf(out, "  %s is revision %d; writing revision %d\n", state.Path, state.Revision, system.ServiceRevision)
			if err := deps.service.Install(ctx, out); err != nil {
				return err
			}
		case changed || (!state.Running && !state.Starting):
			if err := deps.service.Restart(ctx, out); err != nil {
				return err
			}
		default:
			printf(out, "  installed and running (%s)\n", state.Path)
		}
	}

	// 6 — doctor.
	step("6/6 doctor")
	doctorErr := deps.doctor(cmd)

	printf(out, "\n%s\n", st.heading("next"))
	if len(liked.held) > 0 {
		printf(out, "  %s  at a terminal: %s's label aliases and settings, which need a person's yes\n", likeLine(f.like), f.like)
	}
	if cfg, err := loadConfig(); err == nil && len(cfg.Repos) == 0 {
		printf(out, "  latem repo add <owner/name>         allow a repository; the agent picks it up\n")
	}
	printf(out, "  latem workflows check --repo <key>  what this machine can run of it, and the fix for the rest\n")
	printf(out, "  latem secrets set NAME              a value its workflows read (--repo, --env to scope it)\n")
	return doctorErr
}

// setupRepositories adds the repositories this machine builds: those --repo
// names, or — on a terminal, with none configured — those it is asked for.
// Each one's workflows are read without a checkout and checked (A57).
func setupRepositories(cmd *cobra.Command, p *prompter, f setupFlags, cfg *config.Config) error {
	out := cmd.OutOrStdout()
	asking := len(f.repos) == 0 && len(cfg.Repos) == 0 && interactive(cmd)
	if len(f.repos) == 0 && !asking {
		if len(cfg.Repos) == 0 {
			printf(out, "  none yet; `latem repo add <owner/name>` adds one\n")
		} else {
			printf(out, "  %s\n", strings.Join(slices.Sorted(maps.Keys(cfg.Repos)), ", "))
		}
		return nil
	}

	for i := 0; ; i++ {
		var slug string
		if asking {
			q := "  a repository this machine should build (owner/name), or Enter to skip: "
			if i > 0 {
				q = "  another (owner/name), or Enter to go on: "
			}
			answer, err := p.ask(q)
			if err != nil {
				return err
			}
			if answer == "" {
				break
			}
			if slug, err = parseGitHubSlug(answer); err != nil {
				printf(out, "  %v\n", err)
				continue
			}
		} else {
			if i == len(f.repos) {
				break
			}
			slug = f.repos[i]
		}
		key := repoKeyFor(slug)
		if err := runRepoAdd(cmd, repoAddFlags{slug: slug, key: key, inSetup: true}); err != nil {
			if !asking {
				return err
			}
			printf(out, "  %v\n", err)
			continue
		}
		setupCheckRepo(cmd, p, asking, key)
	}
	return nil
}

// setupCheckRepo checks a repository just added and says, in one line, how
// many of its jobs this machine can run and what is in the way of the rest
// (LAT-30): nine repositories at twenty lines each pushed step 4's connect
// code out of sight. The detail is `latem workflows check`'s, and `latem setup
// --json`'s. Asked, on a terminal, it offers the [label_aliases] and [actions]
// entries that would let more run, naming each, since a yes covers only what
// was shown. Once they are added it checks again, because a job refused for
// its labels is checked for nothing else until a runner carries them. A
// failure to read it is reported and does not stop setup.
func setupCheckRepo(cmd *cobra.Command, p *prompter, asking bool, key string) {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	report := func(err error) { printf(out, "  %s: its workflows were not checked: %v\n", key, err) }

	cfg, err := loadConfig()
	if err != nil {
		report(err)
		return
	}
	files, err := readRepo(ctx, cfg, key, fromMirrorThenRemote)
	if err != nil {
		report(err)
		return
	}
	defer files.close()
	empty, err := os.MkdirTemp("", "latem-check-workspace-")
	if err != nil {
		report(err)
		return
	}
	defer func() { _ = os.RemoveAll(empty) }()
	check := func() (checkReport, error) {
		cfg, err := loadConfig()
		if err != nil {
			return checkReport{}, err
		}
		target, err := resolveCheckTarget(ctx, cfg, "", workflowsCheckFlags{repo: key})
		if err != nil {
			return checkReport{}, err
		}
		target.fsys, target.where, target.commit, target.workspace = files.fsys, files.where, files.sha, empty
		stores, err := newSecretStores(cfg.Paths)
		if err != nil {
			return checkReport{}, err
		}
		return analyzeWorkflows(ctx, cfg, target, stores)
	}

	for round := 0; ; round++ {
		rep, err := check()
		if err != nil {
			report(err)
			return
		}
		if round == 0 {
			printf(out, "  %s: %s\n", key, repoSummary(key, rep, !asking))
		} else {
			printf(out, "    added them; now %s\n", repoSummary(key, rep, true))
		}
		// Three rounds at most: an entry setup adds can uncover another
		// entry's need, and never a chain longer than labels, then actions.
		if !asking || !rep.hasFixes() || round == 2 {
			return
		}
		printf(out, "    its workflows need, in config.toml:\n")
		for _, l := range suggestedFixLines(rep) {
			printf(out, "      %s\n", l)
		}
		add, err := p.confirm("    add them?", true)
		if err != nil {
			report(err)
			return
		}
		if !add {
			return
		}
		cfg, err := loadConfig()
		if err != nil {
			report(err)
			return
		}
		if _, err := applyWorkflowFixes(config.DerivePaths(configDir()), cfg, rep); err != nil {
			report(err)
			return
		}
	}
}

// repoSummary is a repository's check in one line: "0 of 14 jobs can run
// here; 4 actions to allow, 1 label alias to add, 1 setting left to you —
// see `latem workflows check --repo antescript`". Without fixes the entries
// --fix would add are not counted: they are what is about to be offered.
func repoSummary(key string, rep checkReport, withFixes bool) string {
	s := fmt.Sprintf("%d of %d jobs can run here", rep.jobs()-rep.failed(), rep.jobs())
	var parts []string
	count := func(n int, one, many string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, map[bool]string{true: one, false: many}[n == 1]))
		}
	}
	if withFixes {
		count(len(rep.allowed), "action to allow", "actions to allow")
		count(len(rep.aliases), "label alias to add", "label aliases to add")
	}
	count(len(leftToYou(rep)), "setting left to you", "settings left to you")
	if len(parts) > 0 {
		s += "; " + strings.Join(parts, ", ")
	}
	if rep.failed() > 0 || rep.broken() > 0 {
		s += fmt.Sprintf(" — see `latem workflows check --repo %s`", key)
	}
	return s
}

// leftToYou is the config.toml settings that would let more of the refused
// jobs run and that setup does not add: whether to run remote actions
// unisolated, or actions not pinned to a commit, is the operator's call, and
// so is which runner a label no runner here carries should mean. A label
// --fix would alias is not one of them, however the finding quotes it. The
// other fix for each — a change to the workflow — is workflows check's to
// say.
func leftToYou(rep checkReport) []string {
	var lines []string
	for _, wr := range rep.workflows {
		for _, j := range wr.jobs {
			if j.level() != levelFail {
				continue
			}
			for _, f := range j.findings {
				for _, fix := range f.fixes {
					i := strings.Index(fix, "[actions] ")
					if k := strings.Index(fix, "[label_aliases] "); k >= 0 && (i < 0 || k < i) {
						i = k
					}
					if i < 0 {
						continue
					}
					l := fix[i:]
					if label, ok := aliasLineLabel(l); ok {
						if _, offered := rep.aliases[label]; offered {
							continue
						}
					} else if strings.HasPrefix(l, "[actions] allowed") {
						continue
					}
					if !slices.Contains(lines, l) {
						lines = append(lines, l)
					}
				}
			}
		}
	}
	return lines
}

// aliasLineLabel is the label a "[label_aliases] <label> = …" line names,
// quoted or not.
func aliasLineLabel(l string) (string, bool) {
	rest, ok := strings.CutPrefix(l, "[label_aliases] ")
	if !ok {
		return "", false
	}
	k, _, ok := strings.Cut(rest, " = ")
	if !ok {
		return "", false
	}
	if u, err := strconv.Unquote(k); err == nil {
		k = u
	}
	return k, true
}

// registered reports whether the machine already has a control plane and a
// token it can read.
func registered(cfg *config.Config) bool {
	if !cfg.Broker.Remote.Enabled || cfg.Broker.Remote.URL == "" {
		return false
	}
	_, err := remote.ReadToken(cfg.Paths.TokenFile)
	return err == nil
}

// writeSetupConfig is `latem init` with no repositories, which setup's config
// step is: the same template, the same modes, the same proof that it loads.
func writeSetupConfig(paths config.Paths, f setupFlags, goos string, docker bool, dockerHost string, dockerSlots int) error {
	name := f.name
	if name == "" {
		if name = defaultMachineName(); name == "" {
			name = "latem"
		}
	}
	body := renderInitConfig(initParams{
		Machine: name, GOOS: goos, GOARCH: runtime.GOARCH, Refs: []string{"main"},
		Docker: docker, DockerHost: dockerHost, Image: f.image, DockerConcurrency: dockerSlots,
	})
	if _, err := config.Parse([]byte(body), config.Options{Dir: paths.Dir}); err != nil {
		return fmt.Errorf("the config setup would write does not load — this is a bug in `latem setup`:\n%w", err)
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", paths.Dir, err)
	}
	if err := os.Chmod(paths.Dir, 0o700); err != nil { //nolint:gosec // §8 requires 0700 on the config directory
		return fmt.Errorf("securing %s: %w", paths.Dir, err)
	}
	return writeFileAtomic(paths.File, []byte(body), 0o600)
}

// dockerEngine names the engine at a socket, for a person.
func dockerEngine(host string) string {
	if e := readiness.Engine(host); e != "" {
		return e
	}
	return "a Docker daemon"
}

// systemService is the real service manager, through internal/system.
type systemService struct{ sys *system.System }

func (s systemService) Status(ctx context.Context) (system.ServiceState, error) {
	return s.sys.ServiceStatus(ctx)
}

func (s systemService) Install(ctx context.Context, w io.Writer) error {
	spec, err := serviceSpec(s.sys, "")
	if err != nil {
		return err
	}
	if err := carryDockerHost(s.sys, w, false); err != nil {
		return err
	}
	rep, err := s.sys.InstallService(ctx, spec, system.InstallOptions{})
	printServiceReport(w, rep, false)
	return err
}

func (s systemService) Restart(ctx context.Context, w io.Writer) error {
	rep, err := s.sys.RestartService(ctx)
	printServiceReport(w, rep, false)
	return err
}

// runDoctorFor is `latem doctor` as a step: its report, and its verdict.
func runDoctorFor(cmd *cobra.Command) error {
	rep := newDoctor().run(cmd.Context())
	out := cmd.OutOrStdout()
	printReport(out, rep, false, useColor(out))
	if _, _, failed, _ := rep.counts(); failed > 0 {
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("doctor: %d check(s) failed; `latem doctor --fix` lists the fixes", failed)}
	}
	return nil
}
