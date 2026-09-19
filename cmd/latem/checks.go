package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/appstore"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor/docker"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/metrics"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// gitMinimum is the git release that introduced --end-of-options.
//
// RUNNER_SPEC amendment A7 requires it in every argument position holding a
// ref, because `--` there means "a pathspec follows" and does not stop option
// parsing the way INV-3 needs. An older git makes the argument-injection
// defence quietly weaker, which is not a property to discover from a build.
var gitMinimum = [3]int{2, 24, 0}

// clockSkewWarn and clockSkewFail bound the acceptable difference between this
// machine's clock and the broker's.
//
// A machine token is validated against a signing window. Below a minute
// nothing breaks; past five, token validation fails with an error that names
// authentication and sends an operator to regenerate a token that was never
// the problem.
const (
	clockSkewWarn = time.Minute
	clockSkewFail = 5 * time.Minute
)

// dockerProbe is the slice of the Docker executor `doctor` needs.
//
// It is an interface so every Docker check runs in a test on a machine with no
// daemon. The base-image contract in particular is the check most worth
// testing and the least testable against a real daemon: it needs an image that
// deliberately violates §13.1.
type dockerProbe interface {
	Preflight(ctx context.Context) error
	CheckImage(ctx context.Context, ref string) (docker.ImageReport, error)
	Close() error
}

// secretResolver is the slice of the secrets chain `doctor` needs.
type secretResolver interface {
	Resolve(ctx context.Context, name string) (secrets.Value, error)
	Providers() []string
}

// doctor holds everything a run of the checks needs.
type doctor struct {
	sys  *system.System
	dir  string
	file string

	cfg    *config.Config
	cfgErr error

	includeSecrets bool

	loadCfg    func() (*config.Config, error)
	newDocker  func(docker.Options) (dockerProbe, error)
	newSecrets func(secrets.Options) (secretResolver, error)
	sameFS     func(mirrorDir, workDir string) error
	// newAppStore and clock exist for A35's checks: one talks to Apple and the
	// other signs a token, and neither should need a real credential or the
	// wall clock to be covered by a test.
	newAppStore func() *appstore.Client
	clock       func() time.Time
	// kvm is injectable for the same reason as the rest: the states worth
	// checking are a host with no /dev/kvm, one whose device the agent's user
	// cannot open, and one that works — and none of the three can be arranged
	// on the machine running the tests.
	kvm func() system.KVM
	// freeGB is how much room the config directory's filesystem has.
	//
	// Injectable for the reason every probe here is: the states worth checking
	// are a disk above a runner's floor and one below it, and a test cannot
	// arrange either — it gets whatever the machine running it has. Asserting
	// that a 40 GB floor passes is asserting that the test machine has 40 GB
	// free, which a developer's laptop does and a CI runner does not.
	freeGB func(context.Context) (float64, error)
	// latestRelease is §20's feed, injectable so the check can be tested
	// without reaching api.github.com — and so a test can arrange the three
	// answers that matter: current, newer, and unreachable.
	latestRelease func(context.Context) (selfupdate.Release, error)
	// kegRubies is [kegOnlyRubies], overridable.
	//
	// It is a list of absolute paths that [system.System.ToolAt] stats on the
	// real filesystem, which made the keg-only case untestable in the way that
	// matters: the test passed on a laptop with Homebrew's ruby installed at
	// /opt/homebrew/opt/ruby/bin/ruby and failed on every CI runner, because
	// it was reading the developer's machine rather than a fixture. A test that
	// asserts on what the host happens to have is a test that reports on the
	// host.
	kegRubies []string
}

func newDoctor() *doctor {
	return &doctor{
		sys: system.New(system.Options{
			// A repo probe must never sit waiting for a username on a terminal
			// nobody is watching. Both variables turn a missing credential
			// into an immediate, reportable failure.
			Env: []string{
				"GIT_TERMINAL_PROMPT=0",
				"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=accept-new",
			},
		}),
		dir:     configDir(),
		file:    configFile(),
		loadCfg: loadConfig,
		newDocker: func(opts docker.Options) (dockerProbe, error) {
			return docker.New(opts)
		},
		newSecrets: func(opts secrets.Options) (secretResolver, error) {
			return secrets.New(opts)
		},
		newAppStore: func() *appstore.Client { return &appstore.Client{} },
		kvm:         system.CheckKVM,
		sameFS:      source.CheckSameFilesystem,
	}
}

// run executes every check and returns the report.
//
// No check may stop the run. An operator on a fresh Mac mini is supposed to
// get the whole list and do one afternoon's work, not fix-rerun-fix-rerun,
// so a probe that panics or errors becomes a failed check rather than an
// aborted diagnosis.
func (d *doctor) run(ctx context.Context) report {
	d.cfg, d.cfgErr = d.loadCfg()

	var rep report
	rep.section("Tooling", d.checkTooling(ctx)...)
	rep.section("Mobile toolchain", d.checkMobileToolchain(ctx)...)
	rep.section("Configuration", d.checkConfiguration(ctx)...)
	rep.section("Disk", d.checkDisk(ctx)...)
	rep.section("Executors", d.checkExecutors(ctx)...)
	rep.section("Repositories", d.checkRepos(ctx)...)
	if d.includeSecrets {
		rep.section("Secrets", d.checkSecrets(ctx)...)
	}
	if d.sys.IsMacOS() {
		rep.section("macOS session", d.checkMacOS(ctx)...)
	}
	rep.section("Service", d.checkService(ctx)...)
	rep.section("Broker", d.checkBroker(ctx)...)
	rep.section("Updates", d.checkSelfUpdate(ctx)...)
	return rep
}

// runnersFor returns the configured runners using an executor.
func (d *doctor) runnersFor(ex config.Executor) []*config.Runner {
	if d.cfg == nil {
		return nil
	}
	var out []*config.Runner
	for _, r := range d.cfg.Runners {
		if r.Executor == ex {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- tooling

func (d *doctor) checkTooling(ctx context.Context) []check {
	out := []check{d.checkGit(ctx)}
	out = append(out, d.checkDocker(ctx)...)
	out = append(out, d.checkTart(ctx))
	if d.sys.IsLinux() {
		out = append(out, d.checkKVM())
	}
	return out
}

func (d *doctor) checkGit(ctx context.Context) check {
	git := d.sys.Git(ctx)
	switch {
	case !git.Found:
		return fail("git.present", "git", "not on PATH", d.installCommand("git"))
	case git.Version.IsZero():
		return warn("git.version", "git", git.Banner+" — could not read a version from this banner")
	case !git.Version.AtLeast(gitMinimum[0], gitMinimum[1], gitMinimum[2]):
		return fail("git.version", "git",
			fmt.Sprintf("%s — too old; --end-of-options needs git %d.%d+", git.Banner, gitMinimum[0], gitMinimum[1]),
			d.installCommand("git")).
			withNotes("INV-3 passes every ref through --end-of-options so a ref cannot become a git option. " +
				"Without it, a ref like --upload-pack=/bin/sh is an argument injection (amendment A7).")
	default:
		return pass("git.version", "git", git.Banner)
	}
}

func (d *doctor) checkDocker(ctx context.Context) []check {
	dockerRunners := d.runnersFor(config.ExecutorDocker)
	required := len(dockerRunners) > 0

	cli := d.sys.Docker(ctx)
	if !cli.Found {
		detail := "not on PATH"
		if !required {
			return []check{warn("docker.present", "docker", detail+" (no docker runner configured)", d.installCommand("docker"))}
		}
		return []check{fail("docker.present", "docker",
			fmt.Sprintf("%s, but %d docker runner(s) are configured", detail, len(dockerRunners)),
			d.installCommand("docker"))}
	}
	out := []check{pass("docker.present", "docker", cli.Banner)}

	// The daemon, not the CLI. A CLI with no daemon behind it is the single
	// most common "docker is installed" false positive.
	probe, err := d.newDocker(docker.Options{})
	if err != nil {
		out = append(out, fail("docker.daemon", "docker daemon", err.Error(), d.startDockerCommand()))
	} else {
		defer func() { _ = probe.Close() }()
		if err := probe.Preflight(ctx); err != nil {
			level := fail
			if !required {
				level = warn
			}
			out = append(out, level("docker.daemon", "docker daemon", firstLineOf(err.Error()), d.startDockerCommand()))
		} else {
			out = append(out, pass("docker.daemon", "docker daemon", "reachable"))
		}
	}

	if d.sys.IsLinux() {
		in, err := d.sys.InDockerGroup(ctx)
		switch {
		case err != nil:
			out = append(out, warn("docker.group", "docker group", "could not read group membership: "+err.Error()))
		case !in:
			out = append(out, fail("docker.group", "docker group",
				fmt.Sprintf("%s is not in the docker group", d.sys.User()),
				fmt.Sprintf("sudo usermod -aG docker %s", d.sys.User()),
				"newgrp docker   # or log out and back in").
				withNotes("A shell that was open before the group was added still does not have it. "+
					"The error this produces names the socket, not the group."))
		default:
			out = append(out, pass("docker.group", "docker group", d.sys.User()+" can reach the daemon socket"))
		}
	}
	return out
}

func (d *doctor) checkTart(ctx context.Context) check {
	tartRunners := d.runnersFor(config.ExecutorTart)
	if !d.sys.IsMacOS() {
		if len(tartRunners) > 0 {
			return fail("tart.present", "tart",
				fmt.Sprintf("a tart runner is configured but tart is macOS-only (this is %s)", d.sys.OS()))
		}
		return skip("tart.present", "tart", "macOS only")
	}
	tart := d.sys.Tart(ctx)
	switch {
	case tart.Found && !tart.Version.IsZero():
		return pass("tart.present", "tart", "tart "+tart.Version.String())
	case tart.Found:
		return pass("tart.present", "tart", tart.Banner)
	case len(tartRunners) > 0:
		return fail("tart.present", "tart",
			fmt.Sprintf("not on PATH, but runner %q uses the tart executor", tartRunners[0].Name),
			"brew install cirruslabs/cli/tart")
	default:
		// No tart runner is configured and tart is not installed. Since
		// amendment A31 that is the default configuration, not a degraded one,
		// so this is a skip rather than a warning: there is no tart runner, so
		// there is nothing here to check. Warning on the shipped default is how
		// operators learn to read past warnings.
		//
		// Whether native is the *right* executor for this machine is a question
		// about who can influence the refs it builds (§13.0), which doctor
		// cannot answer by inspecting the machine. It belongs in the spec and
		// the README, not in a check that would fire identically on every host.
		return skip("tart.present", "tart", "not installed, and no runner uses it")
	}
}

func (d *doctor) checkKVM() check {
	probe := d.kvm
	if probe == nil {
		probe = system.CheckKVM
	}
	k := probe()
	switch {
	case k.Err != nil:
		return warn("kvm.device", "/dev/kvm", "could not be inspected: "+k.Err.Error())
	case !k.Present:
		return warn("kvm.device", "/dev/kvm", "absent — Android emulator jobs cannot run here",
			"sudo modprobe kvm_intel   # or kvm_amd").
			withNotes("Only emulator jobs need this. Whether one is configured cannot be known from " +
				"local config: runs_on and the steps live in jobs.yaml at the resolved commit (INV-5).")
	case !k.Usable:
		return warn("kvm.device", "/dev/kvm", fmt.Sprintf("present but %s cannot open it read-write", d.sys.User()),
			fmt.Sprintf("sudo usermod -aG kvm %s", d.sys.User()),
			"newgrp kvm   # or log out and back in").
			withNotes("An emulator opens the device read-write, so a read-only check passes and the " +
				"emulator still fails.")
	default:
		return pass("kvm.device", "/dev/kvm", "present and writable")
	}
}

// ---------------------------------------------------------- configuration

func (d *doctor) checkConfiguration(ctx context.Context) []check {
	var out []check

	if d.cfgErr != nil {
		problems := config.Problems(d.cfgErr)
		if len(problems) == 0 {
			problems = []error{d.cfgErr}
		}
		// One check per problem, so each gets its own line and its own fix.
		// internal/config already writes the remediation into the message —
		// restating it here in different words would let the two drift.
		for i, p := range problems {
			out = append(out, fail(fmt.Sprintf("config.problem.%d", i+1), "config.toml", p.Error()))
		}
	} else {
		out = append(out, pass("config.load", "config.toml",
			fmt.Sprintf("%s — %d repo(s), %d runner(s)", d.file, len(d.cfg.Repos), len(d.cfg.Runners))))
	}

	out = append(out, d.checkConfigLoadedByAgent(ctx))
	out = append(out, d.checkModes()...)
	out = append(out, d.checkMetricsAddr())
	if c, ok := d.checkWorkDirLocation(); ok {
		out = append(out, c)
	}
	return out
}

// checkConfigLoadedByAgent reports a config.toml the running agent has not read.
//
// The agent loads its configuration once, at start, because §8 makes a bad one
// fatal there rather than mid-run. The cost is that an edit does nothing until
// a restart — and until this check existed, `doctor` reported on the file while
// the agent ran on what it had loaded, so adding a repo by hand gave a green
// `[ ok ] repo / <name>` followed immediately by a run rejected with
// E_UNKNOWN_REPO.
//
// That is the worst shape a diagnostic can take. `doctor` exists to catch drift
// before a run does, and this was drift in latem's own state, which `doctor` is
// better placed to see than anything else on the machine.
//
// The comparison is the config's mtime against the agent process's start time,
// because those are both knowable without asking the agent anything: the health
// the agent reports over the socket carries runners, not repos, so it cannot
// answer "is this the file you read?" today.
func (d *doctor) checkConfigLoadedByAgent(ctx context.Context) check {
	const id, title = "config.loaded", "config reloaded"

	info, err := os.Stat(d.file)
	if err != nil {
		return skip(id, title, "config.toml not readable")
	}

	st, err := d.sys.ServiceStatus(ctx)
	switch {
	case err != nil:
		return skip(id, title, "could not read the service state")
	case !st.Installed:
		return skip(id, title, "no agent service installed; nothing is holding an older config")
	case !st.Running:
		return skip(id, title, "the agent is not running; it will read this config when it starts")
	}

	started, ok := d.agentStartTime(ctx, st)
	if !ok {
		return skip(id, title, "could not determine when the agent started")
	}
	if info.ModTime().After(started) {
		return warn(id, title,
			fmt.Sprintf("config.toml was modified %s after the agent started; the agent is running the configuration it loaded at %s",
				info.ModTime().Sub(started).Round(time.Second), started.Format("15:04:05")),
			"latem service restart").
			withNotes("Everything this command reports about the configuration is read from the " +
				"file, not from the agent. Until it restarts, a repo added here is still " +
				"E_UNKNOWN_REPO to a run.")
	}
	return pass(id, title, "the agent started after the last change to config.toml")
}

// agentStartTime asks the operating system when the agent's process began.
//
// From the pid the service manager reports, because the agent does not publish
// its start time anywhere a local caller can reach without the metrics endpoint,
// which is optional and off on some machines.
func (d *doctor) agentStartTime(ctx context.Context, st system.ServiceState) (time.Time, bool) {
	if st.PID <= 0 {
		return time.Time{}, false
	}
	out, err := d.sys.Exec(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(st.PID))
	if err != nil {
		return time.Time{}, false
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return time.Time{}, false
	}
	// `ps -o lstart=` is the one format both macOS and Linux print here.
	t, err := time.ParseInLocation("Mon Jan  2 15:04:05 2006", text, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// checkModes enforces RUNNER_SPEC §23's directory and file modes.
//
// A world-readable ~/.latem holding a machine token and every secret is the
// whole threat model failing quietly, which is why the agent refuses to start
// rather than warning. `doctor` should therefore say so before the operator
// discovers it from a service that will not come up.
func (d *doctor) checkModes() []check {
	type target struct {
		id, title, path string
		want            fs.FileMode
	}
	targets := []target{
		{"modes.dir", "~/.latem mode", d.dir, 0o700},
		{"modes.config", "config.toml mode", d.file, 0o600},
	}
	if d.cfg != nil {
		targets = append(targets,
			target{"modes.token", "token file mode", d.cfg.Paths.TokenFile, 0o600},
			target{"modes.secrets", "secrets.env mode", d.cfg.Paths.SecretsEnv, 0o600},
			target{"modes.socket", "agent.sock mode", d.cfg.Paths.Socket, 0o600},
		)
	} else {
		// Without a parsed config the conventional names are the best that can
		// be done, and saying so is better than skipping the check on exactly
		// the machine that needs it.
		targets = append(targets,
			target{"modes.token", "token file mode", filepath.Join(d.dir, "agent.key"), 0o600},
			target{"modes.secrets", "secrets.env mode", filepath.Join(d.dir, "secrets.env"), 0o600},
		)
	}

	out := make([]check, 0, len(targets))
	for _, t := range targets {
		m := system.InspectMode(t.path, t.want)
		switch {
		case m.Err != nil:
			out = append(out, warn(t.id, t.title, fmt.Sprintf("%s: %v", t.path, m.Err)))
		case !m.Exists:
			out = append(out, skip(t.id, t.title, t.path+" does not exist yet"))
		case !m.OK:
			out = append(out, fail(t.id, t.title,
				fmt.Sprintf("%s is %04o, want %04o or stricter", t.path, m.Mode, t.want),
				m.FixCommand()))
		default:
			out = append(out, pass(t.id, t.title, fmt.Sprintf("%s is %04o", t.path, m.Mode)))
		}
	}
	return out
}

func (d *doctor) checkMetricsAddr() check {
	if d.cfg == nil {
		return skip("metrics.loopback", "metrics_addr", "config did not load")
	}
	addr := d.cfg.Agent.MetricsAddr
	if addr == "" {
		return pass("metrics.loopback", "metrics_addr", "disabled")
	}
	if err := metrics.CheckLoopback(addr); err != nil {
		return fail("metrics.loopback", "metrics_addr", err.Error(),
			fmt.Sprintf("set metrics_addr = \"127.0.0.1:%s\" in %s", portOf(addr), d.file)).
			withNotes("INV-1: the agent makes outbound connections and binds nothing routable. " +
				"There is no port to forward and no inbound firewall rule to write.")
	}
	return pass("metrics.loopback", "metrics_addr", addr+" is loopback")
}

// checkWorkDirLocation reports a work_dir outside the config directory.
//
// It matters for more than tidiness. The cache manager is constructed from a
// single Root and derives work/, mirrors/, and caches/ underneath it, so a
// relocated agent.work_dir leaves its disk gate measuring free space on the
// config directory's filesystem rather than on the one the build will fill.
func (d *doctor) checkWorkDirLocation() (check, bool) {
	if d.cfg == nil {
		return check{}, false
	}
	work := d.cfg.Paths.Work
	if strings.HasPrefix(work, d.cfg.Paths.Dir+string(filepath.Separator)) {
		return check{}, false
	}
	return warn("config.workdir", "agent.work_dir",
		fmt.Sprintf("%s is outside %s", work, d.cfg.Paths.Dir)).
		withNotes("Disk figures below are measured on the config directory's filesystem. " +
			"Confirm that this is the same filesystem the workspaces land on."), true
}

// -------------------------------------------------------------------- disk

func (d *doctor) checkDisk(ctx context.Context) []check {
	if d.cfg == nil {
		return []check{skip("disk.free", "free disk", "config did not load")}
	}

	// The agent creates these at startup; doctor creating them costs nothing
	// and lets the same-filesystem check run on a machine that has never run
	// a build — which is exactly the machine this check is for.
	_ = os.MkdirAll(d.cfg.Paths.Work, 0o700)
	_ = os.MkdirAll(d.cfg.Paths.Mirrors, 0o700)

	runners := make(map[string]cache.RunnerConfig, len(d.cfg.Runners))
	for _, r := range d.cfg.Runners {
		runners[r.Name] = cache.RunnerConfig{Executor: string(r.Executor), MinFreeGB: r.MinFreeGB()}
	}
	mgr, err := cache.New(cache.Options{Root: d.cfg.Paths.Dir, Runners: runners})
	if err != nil {
		return []check{fail("disk.free", "free disk", err.Error())}
	}

	freeGB := mgr.FreeGB
	if d.freeGB != nil {
		freeGB = d.freeGB
	}
	free, err := freeGB(ctx)
	if err != nil {
		return []check{fail("disk.free", "free disk", err.Error())}
	}

	out := make([]check, 0, len(d.cfg.Runners)+2)
	for _, r := range d.cfg.Runners {
		floor := r.MinFreeGB()
		id := "disk.free." + r.Name
		title := "free disk / " + r.Name
		detail := fmt.Sprintf("%.1f GB free, floor %.0f GB (%s)", free, floor, r.Executor)
		if free < floor {
			out = append(out, fail(id, title, detail,
				"latem cache prune --aggressive",
				"latem cache ls   # find what is large").
				withNotes("The floor is per runner, not global: 40 GB for docker, 60 for native, "+
					"150 for tart — a Tart image carrying Xcode is around 90 GB before the build "+
					"writes a byte (§14.4). Below it the agent refuses runs with E_DISK rather than "+
					"failing halfway through one."))
		} else {
			out = append(out, pass(id, title, detail))
		}
	}
	if len(d.cfg.Runners) == 0 {
		out = append(out, warn("disk.free", "free disk", fmt.Sprintf("%.1f GB free; no runners configured", free)))
	}

	out = append(out, d.checkSameFilesystem())
	out = append(out, d.checkStaleMirrors(mgr))
	return out
}

// checkSameFilesystem is finding F2, and it is the one nobody discovers alone.
func (d *doctor) checkSameFilesystem() check {
	mirrors, work := d.cfg.Paths.Mirrors, d.cfg.Paths.Work
	if err := d.sameFS(mirrors, work); err != nil {
		return fail("disk.samefs", "mirrors and work_dir", err.Error(),
			fmt.Sprintf("put agent.work_dir on the same filesystem as %s, in %s", mirrors, d.file)).
			withNotes("§12 clones each workspace from the mirror with `git clone --local`, which " +
				"hardlinks the object store. Across filesystems git silently copies it instead — no " +
				"error, no warning, just a multi-gigabyte copy per run and a machine that gets " +
				"slower for reasons nobody attributes correctly.")
	}
	return pass("disk.samefs", "mirrors and work_dir", "one filesystem, so clones hardlink")
}

func (d *doctor) checkStaleMirrors(mgr *cache.Local) check {
	stale, err := mgr.StaleMirrors(cache.StaleMirrorAge)
	if err != nil {
		return warn("disk.mirrors", "stale mirrors", err.Error())
	}
	if len(stale) == 0 {
		return pass("disk.mirrors", "stale mirrors", "none older than 90 days")
	}
	return warn("disk.mirrors", "stale mirrors",
		fmt.Sprintf("%d untouched for 90+ days: %s", len(stale), strings.Join(stale, ", ")),
		fmt.Sprintf("rm -rf %s/<repo>.git   # only if you are sure", d.cfg.Paths.Mirrors)).
		withNotes("Reported, never deleted automatically: a mirror may be the only local copy of a " +
			"repository whose origin has since been renamed, made private, or deleted.")
}

// --------------------------------------------------------------- executors

func (d *doctor) checkExecutors(ctx context.Context) []check {
	if d.cfg == nil {
		return []check{skip("executor.image", "base images", "config did not load")}
	}
	var out []check
	out = append(out, d.checkDockerImages(ctx)...)
	out = append(out, d.checkGPU(ctx)...)
	out = append(out, d.checkTartImages(ctx)...)
	if len(out) == 0 {
		out = append(out, skip("executor.image", "base images", "no runner declares a base image"))
	}
	return out
}

// checkDockerImages runs each default image once against the §13.1 contract.
func (d *doctor) checkDockerImages(ctx context.Context) []check {
	var out []check
	for _, r := range d.runnersFor(config.ExecutorDocker) {
		if r.Docker == nil || r.Docker.DefaultImage == "" {
			continue
		}
		id, title := "image."+r.Name, "base image / "+r.Name
		probe, err := d.newDocker(docker.Options{DefaultImage: r.Docker.DefaultImage})
		if err != nil {
			out = append(out, fail(id, title, err.Error(), d.startDockerCommand()))
			continue
		}
		rep, err := probe.CheckImage(ctx, r.Docker.DefaultImage)
		_ = probe.Close()
		if err != nil {
			out = append(out, fail(id, title,
				fmt.Sprintf("%s: %s", r.Docker.DefaultImage, firstLineOf(err.Error())),
				fmt.Sprintf("docker pull %s", r.Docker.DefaultImage)))
			continue
		}
		if rep.OK() {
			out = append(out, pass(id, title, r.Docker.DefaultImage+" satisfies the §13.1 contract"))
			continue
		}
		c := fail(id, title, fmt.Sprintf("%s does not satisfy the §13.1 contract", r.Docker.DefaultImage))
		for _, f := range rep.Failures() {
			c.Notes = append(c.Notes, f.Name+": "+f.Detail)
		}
		c.Notes = append(c.Notes,
			"An image with no uid 1000 in /etc/passwd leaves $HOME unset or unwritable, and the "+
				"failures land forty minutes into a build as npm EACCES, a Gradle daemon that will "+
				"not start, or CocoaPods failing to write its cache — errors that look like tooling "+
				"bugs and send you to the wrong repository entirely.")
		c.Fix = append(c.Fix,
			"add to your image: RUN useradd -u 1000 -m runner && apt-get install -y bash git ca-certificates")
		out = append(out, c)
	}
	return out
}

func (d *doctor) checkGPU(ctx context.Context) []check {
	var out []check
	for _, r := range d.runnersFor(config.ExecutorDocker) {
		if r.Docker == nil || !r.Docker.GPU {
			continue
		}
		id, title := "gpu."+r.Name, "nvidia toolkit / "+r.Name
		probe, err := d.newDocker(docker.Options{GPU: true})
		if err != nil {
			out = append(out, fail(id, title, err.Error(), d.startDockerCommand()))
			continue
		}
		err = probe.Preflight(ctx)
		_ = probe.Close()
		if err != nil {
			out = append(out, fail(id, title, firstLineOf(err.Error()),
				"sudo apt-get install -y nvidia-container-toolkit",
				"sudo nvidia-ctk runtime configure --runtime=docker",
				"sudo systemctl restart docker").
				withNotes("A GPU runner that fails this accepts no work at all rather than failing "+
					"a build at minute forty (§20)."))
			continue
		}
		out = append(out, pass(id, title, "nvidia-smi runs in a container"))
	}
	return out
}

// checkTartImages reports each Tart base image and its real on-disk size.
func (d *doctor) checkTartImages(ctx context.Context) []check {
	var out []check
	for _, r := range d.runnersFor(config.ExecutorTart) {
		if r.Tart == nil || r.Tart.BaseImage == "" {
			continue
		}
		id, title := "tart.image."+r.Name, "tart image / "+r.Name
		img := d.sys.TartImage(ctx, r.Tart.BaseImage)
		if !img.Exists {
			out = append(out, fail(id, title,
				fmt.Sprintf("%s is not built on this machine", r.Tart.BaseImage),
				"see examples/ for the base image build script").
				withNotes("Until it exists, every Xcode job on this runner fails at Prepare (§13.2)."))
			continue
		}
		floor := r.MinFreeGB()
		detail := fmt.Sprintf("%s occupies %s on disk; runner floor is %.0f GB",
			r.Tart.BaseImage, formatBytes(img.SizeBytes), floor)
		if !img.Measured {
			out = append(out, warn(id, title, r.Tart.BaseImage+" exists but its size could not be measured"))
			continue
		}
		imageGB := float64(img.SizeBytes) / float64(1<<30)
		if imageGB > floor {
			out = append(out, warn(id, title, detail,
				fmt.Sprintf("raise min_free_gb above %.0f under [runner.tart] for %q in %s",
					imageGB, r.Name, d.file)).
				withNotes("A clone diverges from its base as the VM runs, and DerivedData lands on "+
					"top. A floor below the image's own size admits a build this machine cannot "+
					"finish (§13.2)."))
			continue
		}
		out = append(out, pass(id, title, detail))
	}
	return out
}

// ------------------------------------------------------------ repositories

// checkRepos asks each configured remote whether this machine can read it.
//
// `git ls-remote` and not a plain reachability test, because the question is
// not "is the host up" but "do the credentials on this machine work" — the
// deploy key that expired, the ssh agent that is not forwarded into a
// LaunchAgent's session, the token that was rotated. Each of those presents at
// gate 6 as E_SOURCE partway into a run.
func (d *doctor) checkRepos(ctx context.Context) []check {
	if d.cfg == nil {
		return []check{skip("repo.reach", "repositories", "config did not load")}
	}
	if len(d.cfg.Repos) == 0 {
		return []check{warn("repo.reach", "repositories", "no [repos.*] entries; this agent would reject every run")}
	}

	out := make([]check, 0, len(d.cfg.Repos))
	for _, name := range sortedStrings(d.cfg.Repos) {
		repo := d.cfg.Repos[name]
		id, title := "repo."+name, "repo / "+name
		// --end-of-options rather than --, per amendment A7: -- would mean
		// "a pathspec follows" and the URL is not a path.
		outBytes, err := d.sys.Exec(ctx, "git", "ls-remote", "--heads", "--end-of-options", repo.URL)
		if err == nil {
			out = append(out, pass(id, title, repo.URL))
			continue
		}
		detail := gitRemoteError(string(outBytes))
		if detail == "" {
			detail = err.Error()
		}
		notes, fixes := repoFailureAdvice(repo.URL, string(outBytes))
		out = append(out, fail(id, title, fmt.Sprintf("%s — %s", repo.URL, detail), fixes...).
			withNotes(notes...))
	}
	// A34, per repo and from the mirror rather than the remote.
	return append(out, d.checkSchemes(ctx)...)
}

// ----------------------------------------------------------------- secrets

// checkSecrets resolves every locally-known secret name.
//
// Names only, never values (INV-4). What is knowable locally is the set of
// names in [secrets.commands] plus the keys in secrets.env — a job's own
// secrets: block lives in jobs.yaml at the resolved commit and is not on disk
// until a run checks it out, so this cannot be the complete set and does not
// claim to be.
func (d *doctor) checkSecrets(ctx context.Context) []check {
	if d.cfg == nil {
		return []check{skip("secrets.resolve", "secrets", "config did not load")}
	}

	chain, err := d.newSecrets(secrets.Options{
		FilePath: d.cfg.Paths.SecretsEnv,
		Commands: d.cfg.Secrets.Commands,
	})
	if err != nil {
		return []check{fail("secrets.chain", "secrets chain", err.Error())}
	}

	out := []check{pass("secrets.chain", "secrets chain", strings.Join(chain.Providers(), " → "))}

	names, fileErr := d.knownSecretNames()
	if fileErr != nil {
		// The error names a line number and never line content, which is the
		// point: a malformed line in this file is a line someone was putting a
		// credential on.
		out = append(out, fail("secrets.file", "secrets.env",
			fileErr.Error(),
			fmt.Sprintf("# fix the line named above in %s", d.cfg.Paths.SecretsEnv)).
			withNotes("No names are listed from a file that does not parse. Resolving against a "+
				"half-read credential file would report names that are not names."))
	}
	if len(names) == 0 {
		out = append(out, skip("secrets.resolve", "secret names",
			"none declared locally; a job's secrets: block is only readable at the resolved commit"))
		return out
	}

	for _, name := range names {
		id, title := "secret."+name, "secret / "+name
		value, err := chain.Resolve(ctx, name)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			out = append(out, fail(id, title, "no provider holds this name",
				fmt.Sprintf("export LATEM_SECRET_%s=…   # to test", envName(name)),
				fmt.Sprintf("or add it to %s", d.cfg.Paths.SecretsEnv)))
		case err != nil:
			c := fail(id, title, err.Error())
			// A backend's own words are the whole answer to a support question
			// — "op: you are not currently signed in" — and are deliberately
			// absent from the error string, which can reach a broker (INV-4).
			var ce *secrets.CommandError
			if errors.As(err, &ce) && ce.Diagnostic() != "" {
				c.Notes = append(c.Notes, ce.Diagnostic())
			}
			out = append(out, c)
		case len(value) == 0:
			out = append(out, fail(id, title, "resolved to an empty value, which is E_MISSING_SECRET at admission"))
		case logstream.TooShort(value):
			out = append(out, warn(id, title,
				fmt.Sprintf("resolves, but is shorter than %d characters", logstream.MinRedactableLength)).
				withNotes(fmt.Sprintf("Values under %d characters are not registered with the redactor: "+
					"matching a short string turns every log line into a wall of asterisks and makes "+
					"the build unreadable (§16). This value would appear in logs verbatim.",
					logstream.MinRedactableLength)))
		default:
			out = append(out, pass(id, title, "resolves"))
		}
		value.Zero()
	}
	// A35: a set whose members each resolve can still be unusable, and that is
	// a different question from the one above.
	return append(out, d.checkCredentials(ctx)...)
}

// knownSecretNames is every secret name this machine can enumerate locally.
//
// The second return is a secrets.env that would not parse. It is reported as
// its own failure rather than swallowed, because the alternative — enumerating
// whatever the broken file appeared to contain — is what used to print the
// interior lines of a PEM as though they were secret names.
func (d *doctor) knownSecretNames() (names []string, fileErr error) {
	set := map[string]bool{}
	for name := range d.cfg.Secrets.Commands {
		set[name] = true
	}
	if path := d.cfg.Paths.SecretsEnv; path != "" {
		declared, err := secrets.NewFileProvider(path).Names()
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			// No secrets file. A machine that keeps everything in the keychain
			// or in [secrets.commands] is not misconfigured.
		case err != nil:
			fileErr = err
		default:
			for _, name := range declared {
				set[name] = true
			}
		}
	}
	return sortedStrings(set), fileErr
}

func envName(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// ------------------------------------------------------------ macOS session

func (d *doctor) checkMacOS(ctx context.Context) []check {
	return []check{
		d.checkLaunchContext(ctx),
		d.checkAgentNotDaemon(),
		d.checkAutoLogin(ctx),
		d.checkFileVault(ctx),
		d.checkKeychain(ctx),
		d.checkXcode(ctx),
		// A33. Last in this section because it is the only one that probes the
		// shell a step will actually get rather than the machine's state.
		d.checkLocale(ctx),
	}
}

func (d *doctor) checkLaunchContext(ctx context.Context) check {
	lc, err := d.sys.LaunchContext(ctx)
	switch {
	case err != nil:
		return warn("macos.session", "launchd context", "could not be read: "+err.Error())
	case lc == system.LaunchContextAqua:
		return pass("macos.session", "launchd context", "Aqua — a GUI session with keychain access")
	case lc == system.LaunchContextSystem:
		return fail("macos.session", "launchd context", "System — this is a daemon context",
			"latem service uninstall",
			"latem service install   # installs a LaunchAgent under ~/Library/LaunchAgents").
			withNotes("codesign needs a real user session with keychain access. In a daemon context " +
				"it fails with errors that point nowhere useful, and this is the single most common " +
				"install failure (§13.3, §23).")
	default:
		return warn("macos.session", "launchd context",
			lc.String()+" — no GUI session, so codesign will not reach the login keychain").
			withNotes("Running doctor over ssh reports this even on a correctly configured machine. " +
				"What matters is the context the agent runs in, not this shell.")
	}
}

// checkAgentNotDaemon looks for a plist in the daemon locations.
//
// §23 puts this first among its three requirements, so a machine that has one
// installed in the wrong place should hear about it even when doctor happens
// to be running from a terminal.
func (d *doctor) checkAgentNotDaemon() check {
	daemonPaths := []string{
		"/Library/LaunchDaemons/" + system.ServiceLabel + ".plist",
		"/Library/LaunchAgents/" + system.ServiceLabel + ".plist",
	}
	for _, p := range daemonPaths {
		if _, err := os.Stat(p); err == nil {
			return fail("macos.daemon", "LaunchAgent location", p+" exists",
				"sudo launchctl bootout system/"+system.ServiceLabel,
				"sudo rm "+p,
				"latem service install").
				withNotes("A system-wide plist runs the agent outside your login session. " +
					"It belongs in ~/Library/LaunchAgents and nowhere else.")
		}
	}
	return pass("macos.daemon", "LaunchAgent location", "no system-wide plist; ~/Library/LaunchAgents is correct")
}

func (d *doctor) checkAutoLogin(ctx context.Context) check {
	al, err := d.sys.AutoLogin(ctx)
	switch {
	case err != nil:
		return warn("macos.autologin", "auto-login", "could not be read: "+err.Error())
	case !al.Enabled:
		return warn("macos.autologin", "auto-login", "not configured — no session exists after a reboot").
			withManual("System Settings → Users & Groups → Automatic login → " + d.sys.User()).
			withNotes("A LaunchAgent needs a logged-in user. Without auto-login the machine boots, " +
				"nobody logs in, the agent never starts, and it goes offline until somebody walks " +
				"over to it (§23). It does not stop this machine building now, which is why it " +
				"is a warning rather than a failure: it stops it coming back by itself.")
	case al.User != d.sys.User():
		return warn("macos.autologin", "auto-login",
			fmt.Sprintf("auto-logs in as %q but the agent runs as %q", al.User, d.sys.User())).
			withManual("System Settings → Users & Groups → Automatic login → " + d.sys.User()).
			withNotes("This looks like a working configuration and is not: the LaunchAgent belongs " +
				"to a user who is never logged in. As above, it costs you the reboot rather " +
				"than the build.")
	default:
		return pass("macos.autologin", "auto-login", "enabled for "+al.User)
	}
}

// checkFileVault is finding F13, and it is the one that quietly falsifies
// "survives reboots".
//
// Both answers are legitimate and the trade is genuinely the operator's — a
// machine holding signing certificates is a reasonable thing to want encrypted
// — so this names both and picks neither. What it will not do is let it be a
// surprise.
func (d *doctor) checkFileVault(ctx context.Context) check {
	status, err := d.sys.FileVault(ctx)
	switch status {
	case system.FileVaultOff:
		return pass("macos.filevault", "FileVault", "off — an unattended reboot reaches the login window")
	case system.FileVaultOn:
		return warn("macos.filevault", "FileVault", "on — an unattended reboot stops before login",
			"sudo fdesetup authrestart   # reboot now, unlocking the disk for exactly this boot",
			"sudo fdesetup disable       # or turn it off, if this machine is physically secure").
			withNotes(
				"With FileVault on, a reboot stops at the pre-boot unlock screen: no login, no "+
					"session, no LaunchAgent, no builds — until somebody walks over and types a "+
					"password. Nothing appears in any log, because the agent never started to write "+
					"one. The symptom is \"my machine went offline overnight and nothing explains it\".",
				"Two options, and this is the operator's decision rather than latem's. Disable "+
					"FileVault, accepting that a stolen machine yields its signing certificates and "+
					"keystores. Or keep it and reboot only via `sudo fdesetup authrestart`, which "+
					"unlocks the disk for exactly the next boot — including from ssh, so remote "+
					"reboots stay possible. A kernel panic or a power cut still parks the machine.",
				"Documented in docs/ios.md.")
	default:
		detail := "could not be determined"
		if err != nil {
			detail += ": " + err.Error()
		}
		return warn("macos.filevault", "FileVault", detail, "fdesetup status")
	}
}

func (d *doctor) checkKeychain(ctx context.Context) check {
	ok, err := d.sys.KeychainUnlocked(ctx)
	switch {
	case err != nil:
		return warn("macos.keychain", "login keychain", "could not be inspected: "+err.Error())
	case !ok:
		return fail("macos.keychain", "login keychain", "locked — every codesign on this machine will fail",
			"security unlock-keychain ~/Library/Keychains/login.keychain-db",
			"security set-keychain-settings -lut 36000   # stop it re-locking after 10 hours idle").
			withNotes("This is the most common signing failure after a reboot, and it presents as a " +
				"codesign error that names the certificate rather than the keychain. E_SECRET_BACKEND " +
				"exists for exactly this: a machine condition that resolves without editing a commit " +
				"(amendment A16).")
	default:
		return pass("macos.keychain", "login keychain", "unlocked")
	}
}

func (d *doctor) checkXcode(ctx context.Context) check {
	x := d.sys.Xcode(ctx)
	switch {
	case x.Err != nil || x.DeveloperDir == "":
		return warn("macos.xcode", "Xcode", "xcode-select reported nothing",
			"xcode-select --install",
			"sudo xcode-select -s /Applications/Xcode.app/Contents/Developer")
	case !x.IsFullXcode:
		return fail("macos.xcode", "Xcode", x.DeveloperDir+" is the Command Line Tools, not Xcode",
			"sudo xcode-select -s /Applications/Xcode.app/Contents/Developer").
			withNotes("The Command Line Tools cannot build an iOS app. The failure — \"unable to find " +
				"a destination matching the provided destination specifier\" — reads as a project " +
				"problem rather than a machine one.")
	case !x.LicenseAccepted:
		return fail("macos.xcode", "Xcode", x.DeveloperDir+" — the licence has not been accepted",
			"sudo xcodebuild -license accept").
			withNotes("Every build on the machine fails until it is, with a message that only appears " +
				"on the stderr of the first tool to notice.")
	default:
		detail := x.DeveloperDir
		if !x.Version.IsZero() {
			detail = fmt.Sprintf("Xcode %s at %s", x.Version, x.DeveloperDir)
		}
		return pass("macos.xcode", "Xcode", detail+", licence accepted")
	}
}

// ----------------------------------------------------------------- service

func (d *doctor) checkService(ctx context.Context) []check {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return []check{skip("service.installed", "agent service", "no service integration on "+runtime.GOOS)}
	}

	st, err := d.sys.ServiceStatus(ctx)
	if err != nil {
		return []check{warn("service.installed", "agent service", err.Error())}
	}

	var out []check
	switch {
	case !st.Installed:
		out = append(out, warn("service.installed", "agent service",
			"not installed — the agent will not start after a reboot", "latem service install"))
	case !st.Running:
		out = append(out, warn("service.installed", "agent service",
			"installed at "+st.Path+" but not running", "latem service install"))
	default:
		out = append(out, pass("service.installed", "agent service", "running from "+st.Path))
	}

	if d.sys.IsLinux() {
		linger, err := d.sys.Linger(ctx, "")
		switch {
		case err != nil:
			out = append(out, warn("service.linger", "systemd linger", "could not be read: "+err.Error()))
		case !linger:
			out = append(out, fail("service.linger", "systemd linger",
				fmt.Sprintf("disabled for %s — the service stops when you log out", d.sys.User()),
				strings.Join(d.sys.EnableLingerArgv(""), " ")).
				withNotes("A user unit without linger looks perfectly healthy while you are logged "+
					"in and stops the moment you disconnect, which on a headless build machine is "+
					"seconds after the install (§23)."))
		default:
			out = append(out, pass("service.linger", "systemd linger", "enabled for "+d.sys.User()))
		}
	}
	return out
}

// ------------------------------------------------------------------ broker

func (d *doctor) checkBroker(ctx context.Context) []check {
	if d.cfg == nil {
		return []check{skip("broker.clock", "clock skew", "config did not load")}
	}
	if !d.cfg.Broker.Remote.Enabled || d.cfg.Broker.Remote.URL == "" {
		return []check{skip("broker.clock", "clock skew", "no remote broker configured")}
	}

	sk, err := d.sys.ClockSkew(ctx, d.cfg.Broker.Remote.URL)
	if err != nil {
		return []check{warn("broker.clock", "clock skew",
			fmt.Sprintf("%s: %s", d.cfg.Broker.Remote.URL, firstLineOf(err.Error())))}
	}

	detail := fmt.Sprintf("%s off %s", sk.Abs().Round(time.Millisecond), d.cfg.Broker.Remote.URL)
	switch {
	case sk.Abs() >= clockSkewFail:
		return []check{fail("broker.clock", "clock skew", detail, d.clockSyncCommand()).
			withNotes("A machine token is validated against a signing window. This much skew makes " +
				"every connection fail authentication, and the error names authentication — so the " +
				"first thing an operator does is regenerate a token that was never the problem.")}
	case sk.Abs() >= clockSkewWarn:
		return []check{warn("broker.clock", "clock skew", detail, d.clockSyncCommand())}
	default:
		return []check{pass("broker.clock", "clock skew", detail)}
	}
}

// ----------------------------------------------------------------- helpers

// installCommand is the platform's way to install a tool.
func (d *doctor) installCommand(tool string) string {
	if d.sys.IsMacOS() {
		switch tool {
		case "git":
			return "xcode-select --install   # or: brew install git"
		case "docker":
			return "brew install --cask docker"
		}
		return "brew install " + tool
	}
	switch tool {
	case "docker":
		return "curl -fsSL https://get.docker.com | sh"
	default:
		return "sudo apt-get install -y " + tool + "   # or your distribution's equivalent"
	}
}

func (d *doctor) startDockerCommand() string {
	if d.sys.IsMacOS() {
		return "open -a Docker   # and wait for the whale to settle"
	}
	return "sudo systemctl start docker"
}

func (d *doctor) clockSyncCommand() string {
	if d.sys.IsMacOS() {
		return "sudo sntp -sS time.apple.com"
	}
	return "sudo systemctl restart systemd-timesyncd   # or: sudo chronyc makestep"
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// sshProgressLine reports whether a line is something ssh prints on its way to
// succeeding rather than a description of a failure.
//
// "Warning: Permanently added …" is the one that matters. ssh writes it to
// stderr the first time it accepts a host key, which is a success, and it is
// therefore the *first* line of stderr on exactly the machines this check
// exists for — new ones. Reporting the first line verbatim told an operator
// their repository was unreachable because of a known-hosts warning, while the
// line that said "Permission denied (publickey)" sat below it, unread.
func sshProgressLine(line string) bool {
	return strings.HasPrefix(line, "Warning: Permanently added")
}

// gitRemoteError picks the line of `git ls-remote` output that names the
// failure, rather than whichever line ssh happened to write first.
//
// The first non-progress line wins: git and ssh both put the specific cause
// ("Permission denied (publickey).", "ERROR: Repository not found.") above
// their own summary ("fatal: Could not read from remote repository."), so the
// first one that is not progress is the most specific one available. If every
// line is progress — which would mean git failed while saying nothing — the
// first line is still better than an empty detail.
func gitRemoteError(out string) string {
	var fallback string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if fallback == "" {
			fallback = line
		}
		if sshProgressLine(line) {
			continue
		}
		return line
	}
	return fallback
}

// repoFailureAdvice turns what git said into the explanation and the repair for
// it.
//
// One fixed paragraph used to be attached to every failure of this check: that
// an agent started by launchd does not inherit an interactive ssh agent. It
// describes a real trap, and it was the wrong answer for the commonest case by
// far — a machine with no key at all — so an operator whose ~/.ssh did not
// exist went looking at a service's environment for a credential that was
// never anywhere. The note now follows the cause, and the `fix` is a repair
// rather than another diagnostic wherever the cause is specific enough to name
// one.
func repoFailureAdvice(url, out string) (notes, fixes []string) {
	seeFullError := fmt.Sprintf("git ls-remote %s   # to see the full error", url)

	// Assigned rather than returned inline from each arm. A multi-value return
	// of two composite literals is formatted differently by Go 1.26's gofmt and
	// Go 1.27's, and this repository lints with a pinned golangci-lint in CI and
	// whatever is installed locally — so the inline form is a file that is
	// correctly formatted on one machine and not the other.
	switch {
	case strings.Contains(out, "Permission denied (publickey"):
		notes = []string{
			"This machine has no ssh key the host accepts. A deploy key in ~/.ssh with an " +
				"ssh config entry is the form that works from both an interactive shell and an " +
				"agent started by launchd or systemd; a forwarded agent works from neither, " +
				"because the service does not inherit your session.",
		}
		fixes = []string{"gh auth login --git-protocol ssh   # or install a deploy key in ~/.ssh"}

	case strings.Contains(out, "Repository not found"),
		strings.Contains(out, "does not appear to be a git repository"):
		notes = []string{
			"The credential worked and the repository did not answer to it. Either the URL " +
				"names something that is not there, or the key reaching the host is not entitled " +
				"to that repository — a private repo answers both the same way, on purpose, so " +
				"that it does not confirm its own existence.",
		}
		fixes = []string{seeFullError}

	case strings.Contains(out, "Host key verification failed"):
		notes = []string{
			"ssh has no accepted host key for this host and could not ask: an agent has no " +
				"terminal to answer the prompt on. The key has to be in known_hosts before the " +
				"first run rather than during it.",
		}
		fixes = []string{fmt.Sprintf("ssh-keyscan %s >> ~/.ssh/known_hosts", sshHostOf(url))}

	case strings.Contains(out, "Could not resolve hostname"):
		notes = []string{
			"Name resolution failed, so nothing was authenticated and nothing was refused. On " +
				"a machine that builds unattended this is worth distinguishing from a credential " +
				"problem, because it comes back on its own and a credential does not.",
		}
		fixes = []string{seeFullError}

	case strings.Contains(out, "Authentication failed"):
		notes = []string{
			"An https URL was refused. The credential comes from a helper rather than from " +
				"~/.ssh, and a helper that can only answer by prompting cannot answer for a " +
				"service — which is why this can work in your shell and fail in the agent.",
		}
		fixes = []string{seeFullError}

	case strings.Contains(out, "Connection timed out"),
		strings.Contains(out, "Connection refused"),
		strings.Contains(out, "Operation timed out"):
		notes = []string{
			"The host did not complete a connection. A firewall or a network that allows your " +
				"workstation and not this machine presents exactly this way.",
		}
		fixes = []string{seeFullError}

	default:
		notes = []string{
			"An agent started by launchd or systemd does not inherit your interactive ssh " +
				"agent. A deploy key in ~/.ssh with an ssh config entry works from both; a " +
				"forwarded agent works from neither.",
		}
		fixes = []string{seeFullError}
	}

	return notes, fixes
}

// sshHostOf pulls the hostname out of an scp-style or ssh:// URL, for a
// ssh-keyscan line an operator can paste. It returns the URL unchanged when it
// cannot tell, which makes the suggestion obviously wrong rather than subtly
// wrong.
func sshHostOf(url string) string {
	rest := url
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, ":/"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return url
	}
	return rest
}

func portOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i+1:]
	}
	return "9838"
}

// -------------------------------------------------------- mobile toolchain

// rubyMinimum is the Gemfile floor for both examples. Modern fastlane will not
// install against macOS's 2.6.
var rubyMinimum = [3]int{3, 1, 0}

// jdkForAndroid is what AGP 9 accepts. A newer JDK is not "more compatible":
// `brew install gradle` pulls in the current one as a dependency, Gradle picks
// it as the daemon JVM, and AGP rejects it — so installing Gradle can break an
// Android build that worked the day before.
const jdkForAndroid = 17

// checkMobileToolchain covers what the iOS and Android pipelines need and
// nothing else does.
//
// Every check here exists because it cost real time on 2026-08-11
// (manual-baseline.md §7.5). They are warnings rather than failures: a macOS
// runner might build Go, and doctor cannot know what jobs a repo declares —
// jobs.yaml lives in the repo at the resolved commit (§9), not on this machine.
//
// The shape they share is the interesting part. In each case a working tool IS
// installed and the one on PATH is the wrong one, so every naive check
// ("is ruby present?", "does java run?") passes on a machine that cannot build.
func (d *doctor) checkMobileToolchain(ctx context.Context) []check {
	rubyCheck, rubyBin := d.checkRuby(ctx)
	out := []check{
		rubyCheck,
		d.checkBundler(ctx, rubyBin),
		d.checkJDK(ctx),
		d.checkAndroidSDK(),
	}
	if d.sys.IsMacOS() {
		out = append(out, d.checkSimulatorRuntime(ctx))
	}
	return out
}

// kegOnlyRubies are Homebrew's installation prefixes. Homebrew's ruby is
// keg-only and never on PATH by itself, which is the entire trap: `which ruby`
// returns /usr/bin/ruby (2.6) after you have "installed Ruby".
var kegOnlyRubies = []string{
	"/opt/homebrew/opt/ruby/bin/ruby",
	"/usr/local/opt/ruby/bin/ruby",
}

// checkRuby also returns the bin directory of the Ruby a step should use, so
// the bundler check can probe the same one. Empty when there is none.
func (d *doctor) checkRuby(ctx context.Context) (check, string) {
	rb := d.sys.Tool(ctx, "ruby", "--version")

	// A usable Ruby off PATH is the common state and the confusing one, so
	// look for it before reporting on whatever PATH resolved.
	var kegPath, kegBanner string
	for _, p := range d.kegPaths() {
		if t := d.sys.ToolAt(ctx, p, "--version"); t.Found && t.Version.AtLeast(rubyMinimum[0], rubyMinimum[1], rubyMinimum[2]) {
			kegPath, kegBanner = p, t.Banner
			break
		}
	}

	switch {
	case !rb.Found && kegPath == "":
		return warn("ruby.version", "ruby", "not on PATH",
			"brew install ruby").
			withNotes("Only needed for fastlane — the iOS and Android release jobs. " +
				"A runner that builds neither does not need it."), ""

	case rb.Found && rb.Version.AtLeast(rubyMinimum[0], rubyMinimum[1], rubyMinimum[2]):
		return pass("ruby.version", "ruby", rb.Banner), filepath.Dir(rb.Path)

	case kegPath != "":
		// The trap, named precisely.
		found := "not on PATH"
		if rb.Found {
			found = rb.Banner + " at " + rb.Path
		}
		return warn("ruby.version", "ruby",
			fmt.Sprintf("PATH gives %s, but %s is installed", found, kegBanner),
			`export PATH="`+filepath.Dir(kegPath)+`:$PATH"`).
			withNotes("Homebrew's ruby is keg-only, so installing it does not put it on PATH. " +
				"macOS ships 2.6 at /usr/bin/ruby and fastlane will not run on it — the failure is a " +
				"wall of native-extension compile errors naming neither Ruby nor its version. " +
				"A step gets the environment the agent hands it (§9), not an interactive shell's, " +
				"so this must be in the service environment or in the repo's ci/ scripts."), filepath.Dir(kegPath)

	default:
		return warn("ruby.version", "ruby",
			rb.Banner+" — too old; fastlane needs "+fmt.Sprintf("%d.%d+", rubyMinimum[0], rubyMinimum[1]),
			"brew install ruby"), ""
	}
}

// kegPaths is where a usable off-PATH ruby might be.
func (d *doctor) kegPaths() []string {
	if d.kegRubies != nil {
		return d.kegRubies
	}
	return kegOnlyRubies
}

// checkBundler probes bundler belonging to the Ruby a step will actually use,
// not whatever PATH resolves.
//
// The distinction is not pedantic. On a machine whose PATH ruby is macOS's 2.6
// while a usable 3.4 sits keg-only, `bundle --version` reports 2.6's bundler —
// so the naive check PASSES, directly beneath a line saying the Ruby is
// unusable. It reports a working toolchain on a machine that cannot build, and
// it does so more confidently the more broken the machine is.
func (d *doctor) checkBundler(ctx context.Context, rubyBin string) check {
	b := d.sys.Tool(ctx, "bundle", "--version")
	if rubyBin != "" {
		if at := d.sys.ToolAt(ctx, filepath.Join(rubyBin, "bundle"), "--version"); at.Found {
			b = at
		} else if b.Found && filepath.Dir(b.Path) != rubyBin {
			return warn("ruby.bundler", "bundler",
				b.Banner+" at "+b.Path+" — belongs to a different Ruby than "+rubyBin,
				"gem install bundler").
				withNotes("This bundler runs under the Ruby on PATH, which the check above " +
					"reported as the wrong one. `bundle install` under it either fails or " +
					"installs gems fastlane cannot load.")
		}
	}
	if !b.Found {
		return warn("ruby.bundler", "bundler", "not on PATH",
			"gem install bundler").
			withNotes("`bundle install` can succeed and `bundle exec fastlane` still report " +
				"\"command not found: fastlane\" when the gem's binstub lands outside bundler's " +
				"executable directory. Both examples pin gems to vendor/bundle for that reason.")
	}
	return pass("ruby.bundler", "bundler", b.Banner)
}

func (d *doctor) checkJDK(ctx context.Context) check {
	java := d.sys.Tool(ctx, "java", "-version")

	// macOS ships a stub at /usr/bin/java that prints "Unable to locate a Java
	// Runtime" and exits non-zero. It is on PATH and it is not a JDK, so
	// presence alone proves nothing.
	if java.Found && java.Version.IsZero() {
		return warn("java.version", "JDK",
			"java is on PATH but reports no version — on macOS that is Apple's stub, not a JDK",
			fmt.Sprintf("brew install openjdk@%d", jdkForAndroid)).
			withNotes("Only needed for Android. openjdk is keg-only like ruby, so " +
				"JAVA_HOME must be set explicitly; Gradle otherwise picks whatever java resolves to.")
	}
	if !java.Found {
		return warn("java.version", "JDK", "not on PATH",
			fmt.Sprintf("brew install openjdk@%d", jdkForAndroid)).
			withNotes("Only needed for Android release jobs.")
	}
	// BOTH bounds. The first version of this check tested only `> 17` and
	// therefore passed JDK 11 — reported "ok" on a Mac mini that cannot build
	// an Android release at all. A one-sided range check on a value with a
	// floor and a ceiling is the same defect as an assertion that cannot fail:
	// it is most confident exactly where it is least correct.
	switch maj := java.Version.Major; {
	case maj > 0 && maj < jdkForAndroid:
		return warn("java.version", "JDK",
			fmt.Sprintf("%s — too old; AGP 9 needs %d", java.Banner, jdkForAndroid),
			fmt.Sprintf("brew install openjdk@%d", jdkForAndroid)).
			withNotes("Gradle fails with a message about an unsupported class file version, " +
				"which names a number rather than a JDK. openjdk is keg-only, so installing it " +
				"is not enough — JAVA_HOME must point at it, and the examples' ci/lib/env.sh does that.")
	case maj > jdkForAndroid:
		return warn("java.version", "JDK",
			fmt.Sprintf("%s — AGP 9 wants %d; a newer JDK is rejected, not tolerated", java.Banner, jdkForAndroid),
			fmt.Sprintf("brew install openjdk@%d, then set JAVA_HOME to it", jdkForAndroid)).
			withNotes("`brew install gradle` pulls in the current JDK as a dependency and Gradle " +
				"selects it as the daemon JVM, so installing Gradle can break a working Android build.")
	}
	return pass("java.version", "JDK", java.Banner)
}

// androidSDKs are where an SDK lands, in the order the examples' ci/lib/env.sh
// looks. platform-tools is the marker: an SDK root without it is a partial
// install that fails much later.
var androidSDKs = []string{
	"Library/Android/sdk",
}

func (d *doctor) checkAndroidSDK() check {
	if env := d.sys.Getenv("ANDROID_HOME"); env != "" {
		if _, err := os.Stat(filepath.Join(env, "platform-tools")); err == nil {
			return pass("android.sdk", "Android SDK", env+" (ANDROID_HOME)")
		}
		return warn("android.sdk", "Android SDK",
			"ANDROID_HOME is "+env+" but it has no platform-tools/")
	}
	for _, rel := range androidSDKs {
		p := filepath.Join(d.sys.Home(), rel)
		if _, err := os.Stat(filepath.Join(p, "platform-tools")); err == nil {
			return warn("android.sdk", "Android SDK",
				"found at "+p+", but ANDROID_HOME is not set",
				`export ANDROID_HOME="`+p+`"`).
				withNotes("Gradle's own advice is to write the path into local.properties. Do not: " +
					"that file is per-machine and must not be committed, so a runner would need it " +
					"generated per run. The environment variable is the reproducible form.")
		}
	}
	return warn("android.sdk", "Android SDK", "not found",
		"brew install --cask android-commandlinetools").
		withManual("or install the SDK through Android Studio").
		withNotes("Only needed for Android jobs.")
}

// checkSimulatorRuntime is the Mobile toolchain section's macOS-only member.
//
// A simulator runtime is a separate download from Xcode and Xcode ships with
// none, so a machine with a current Xcode can carry only a runtime left over
// from an older one. Nothing warns about it until a build dies inside actool:
//
//	No simulator runtime version from ["21C62"] available to use with
//	iphonesimulator SDK version 23A339
//
// which names two build numbers, neither of which is the version an operator
// would search for, and does not say that anything needs downloading. Found on
// a Mac mini whose Xcode 26 had only an iOS 17.2 runtime — the machine the
// examples were developed on happened to have a matching one, so the whole
// failure mode was invisible there.
func (d *doctor) checkSimulatorRuntime(ctx context.Context) check {
	sdk, err := d.sys.SimulatorSDKVersion(ctx)
	if err != nil {
		return skip("xcode.simruntime", "simulator runtime", "no iphonesimulator SDK to compare against")
	}
	rts, err := d.sys.SimulatorRuntimes(ctx)
	if err != nil {
		return warn("xcode.simruntime", "simulator runtime",
			"could not list runtimes: "+err.Error(),
			"xcrun simctl list runtimes")
	}

	var installed []string
	for _, r := range rts {
		if r.Platform != "iOS" || !r.Available {
			continue
		}
		installed = append(installed, r.Version)
		if r.Major() == sdk.Major {
			return pass("xcode.simruntime", "simulator runtime",
				fmt.Sprintf("iOS %s installed, matching SDK %s", r.Version, sdk))
		}
	}

	// Both halves, deliberately: none at all, and some-but-none-matching. The
	// second is the one that actually happens, and a check written only for the
	// first would have passed on the machine that failed.
	if len(installed) == 0 {
		return warn("xcode.simruntime", "simulator runtime",
			fmt.Sprintf("none installed; the iphonesimulator SDK is %s", sdk),
			"xcodebuild -downloadPlatform iOS").
			withNotes("Any simulator build fails without one, including the credential-free " +
				"smoke job. The download is several GB.")
	}
	return warn("xcode.simruntime", "simulator runtime",
		fmt.Sprintf("installed: iOS %s — none matches the iphonesimulator SDK %s",
			strings.Join(installed, ", "), sdk),
		"xcodebuild -downloadPlatform iOS").
		withNotes("actool reports this as \"No simulator runtime version from [<build>] available " +
			"to use with iphonesimulator SDK version <build>\", naming two build numbers and " +
			"not saying that a download is needed.")
}
