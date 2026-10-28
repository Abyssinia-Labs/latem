package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// haveTrustedKey reports whether this build can verify a release. Indirected
// through a variable for one reason: the accepted direction of the
// `auto_update` rule is unreachable from a test otherwise, because the answer
// comes from a file embedded at build time and this tree's copy is the
// placeholder. A rule with only its refusal tested is half a rule.
var haveTrustedKey = selfupdate.HaveTrustedKey

// validate applies every rule in §8, filling defaults as it goes, and returns
// all the problems rather than the first.
//
// Defaults are applied inside validation rather than before it so that "absent"
// and "explicitly wrong" produce different outcomes: an absent shutdown_grace
// becomes 30m, while `shutdown_grace = "-5m"` is an error naming the key.
func validate(cfg *Config, md *toml.MetaData, opts Options) []error {
	var problems []error

	cfg.Paths = derivePaths(opts.Dir)

	problems = append(problems, validateSchemaVersion(cfg)...)
	problems = append(problems, applyPathOverrides(cfg, opts)...)
	problems = append(problems, validateMachine(cfg)...)
	problems = append(problems, validateAgent(cfg, md, opts)...)
	problems = append(problems, validateBroker(cfg, md)...)
	problems = append(problems, validateRepos(cfg)...)
	problems = append(problems, validateRunners(cfg)...)
	problems = append(problems, validateSecrets(cfg)...)
	if !opts.skipFilesystem {
		problems = append(problems, validateFilesystem(cfg)...)
	}
	return problems
}

// validateSchemaVersion is deliberately the first check and always produces a
// hint about what to do, because a version mismatch is the one failure where
// the rest of the error list is likely to be noise from a schema this binary
// does not speak.
func validateSchemaVersion(cfg *Config) []error {
	switch {
	case cfg.SchemaVersion == SchemaVersion:
		return nil
	case cfg.SchemaVersion == 0:
		return []error{fmt.Errorf("schema_version is missing; add %q as the first line — see RUNNER_SPEC §8",
			fmt.Sprintf("schema_version = %d", SchemaVersion))}
	case cfg.SchemaVersion > SchemaVersion:
		return []error{fmt.Errorf("schema_version is %d but this agent understands %d; the file was written for a newer latem — upgrade the agent (`latem version` reports what is installed)",
			cfg.SchemaVersion, SchemaVersion)}
	default:
		return []error{fmt.Errorf("schema_version is %d but this agent understands %d; if the file predates this release, see RUNNER_SPEC §8 for the current schema and set schema_version = %d once the file matches it",
			cfg.SchemaVersion, SchemaVersion, SchemaVersion)}
	}
}

func validateMachine(cfg *Config) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if cfg.Machine.Name != "" && !machineNamePattern.MatchString(cfg.Machine.Name) {
		add("machine.name %q must match %s — it is display-only, but it reaches terminals and log lines",
			cfg.Machine.Name, machineNamePattern)
	}
	// An empty machine.id is normal and supported: the local broker works with
	// no control plane at all (§7.3), and the id is assigned by `latem
	// register`. Refusing to start without one would make an unregistered
	// machine useless for exactly the workflow §19 leads with.
	if cfg.Machine.ID != "" && !machineIDPattern.MatchString(cfg.Machine.ID) {
		add("machine.id %q must match %s; it is assigned by `latem register` and should not be edited by hand",
			cfg.Machine.ID, machineIDPattern)
	}
	problems = append(problems, validateLabels("machine.labels", cfg.Machine.Labels)...)
	return problems
}

func validateAgent(cfg *Config, md *toml.MetaData, opts Options) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	problems = appendDuration(problems, "agent.shutdown_grace", &cfg.Agent.ShutdownGrace, DefaultShutdownGrace)
	problems = appendDuration(problems, "agent.log_retention", &cfg.Agent.LogRetention, DefaultLogRetention)
	problems = appendDuration(problems, "agent.queue_ttl", &cfg.Agent.QueueTTL, DefaultQueueTTL)

	switch cfg.Agent.RemoteLogs {
	case "":
		cfg.Agent.RemoteLogs = RemoteLogsAlways
	case RemoteLogsAlways, RemoteLogsOnFailure:
	default:
		add("agent.remote_logs is %q; it must be %q or %q (§7.4)",
			cfg.Agent.RemoteLogs, RemoteLogsAlways, RemoteLogsOnFailure)
	}

	/*
	 * `auto_update = true` is accepted only by a build that could act on it.
	 *
	 * §20's self-update now exists — internal/selfupdate checks a release
	 * feed, verifies a minisign signature over the checksums manifest, and
	 * internal/agent replaces the binary and re-execs when no run is active.
	 * What a *particular binary* may still lack is the trust root: the
	 * signing key is embedded at build time from
	 * internal/selfupdate/trusted.pub, and a build made before that file was
	 * populated can verify nothing.
	 *
	 * So the refusal narrowed rather than disappeared, and the reason it did
	 * not simply disappear is the one this project keeps naming: A27's `latem
	 * approve` moved a row and recorded nothing; A38's documented `~` was
	 * accepted by no executor. A control that reads as a control and does
	 * nothing is worse than an absent one, because the operator stops
	 * looking. An agent that checked a feed daily and declined every release
	 * for want of a key would be exactly that — and it would do it in a log
	 * nobody is reading, which is worse than a load error nobody can miss.
	 */
	if cfg.Agent.AutoUpdate && !haveTrustedKey() {
		add("agent.auto_update is true, and this build trusts no release signing key (§20): " +
			"it was built with an unpopulated internal/selfupdate/trusted.pub, so it could " +
			"verify no release and would decline every one — remove the line, or install a " +
			"signed build from a release")
	}

	// Absent means the documented default; an explicit "" disables metrics.
	// Deleting the line is how an operator gets §20's default, not how they
	// turn the listener off, and conflating the two would silently open a
	// socket on a machine whose owner deleted the key to close one.
	if !md.IsDefined("agent", "metrics_addr") {
		cfg.Agent.MetricsAddr = DefaultMetricsAddr
	}
	if cfg.Agent.MetricsAddr != "" {
		if err := validateMetricsAddr(cfg.Agent.MetricsAddr, opts.LookupIP); err != nil {
			problems = append(problems, err)
		}
	}

	return problems
}

// validateMetricsAddr is INV-1 enforced in the parser.
//
// The agent must not bind a listener on a non-loopback interface, and the
// config loader is the only place that can be checked before something binds.
// Everything else about the metrics endpoint is optional; this is not.
//
// Three cases are rejected that look harmless:
//
//   - ":9838" has no host, which binds every interface including the public
//     one. It is the most common way to write a listen address and the most
//     dangerous one here.
//   - a hostname whose resolution includes any non-loopback address, because
//     binding to a name binds to what it resolves to. One loopback answer
//     among several does not make the address safe.
//   - a name that cannot be resolved at all, because "it will probably be
//     loopback" is not a basis on which to open a socket.
func validateMetricsAddr(addr string, lookupIP lookupIPFunc) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf(`agent.metrics_addr %q is not "host:port" (%w); write %q, or "" to disable metrics`,
			addr, err, DefaultMetricsAddr)
	}
	if host == "" {
		return fmt.Errorf(`agent.metrics_addr %q has no host, which binds every interface — INV-1 forbids that; write %q`,
			addr, DefaultMetricsAddr)
	}
	p, perr := strconv.Atoi(port)
	if perr != nil || p < 1 || p > 65535 {
		return fmt.Errorf("agent.metrics_addr %q: port must be a number from 1 to 65535, got %q", addr, port)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("agent.metrics_addr %q binds %s, which is not a loopback address; INV-1 permits loopback only — use %q and point a local Prometheus at it",
				addr, ip, DefaultMetricsAddr)
		}
		return nil
	}

	ips, err := lookupIP(host)
	if err != nil {
		return fmt.Errorf("agent.metrics_addr %q: cannot resolve %q (%w); use a literal loopback address such as %q so the INV-1 check does not depend on a resolver",
			addr, host, err, DefaultMetricsAddr)
	}
	if len(ips) == 0 {
		return fmt.Errorf("agent.metrics_addr %q: %q resolves to nothing; use a literal loopback address such as %q",
			addr, host, DefaultMetricsAddr)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("agent.metrics_addr %q: %q resolves to %s, which is not a loopback address; INV-1 permits loopback only — use %q",
				addr, host, ip, DefaultMetricsAddr)
		}
	}
	return nil
}

func validateBroker(cfg *Config, md *toml.MetaData) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	// The local broker defaults to on. §6 connects it unconditionally, and an
	// agent without it cannot be reached by `latem run`, `latem cancel`, or
	// `latem approve` — which is to say, not at all on a machine with no
	// control plane.
	if !md.IsDefined("broker", "local", "enabled") {
		cfg.Broker.Local.Enabled = true
	}

	r := cfg.Broker.Remote
	if r.URL != "" {
		if err := validateBrokerURL(r.URL); err != nil {
			problems = append(problems, err)
		}
	}
	if r.Enabled {
		if r.URL == "" {
			add("broker.remote.enabled is true but broker.remote.url is empty; set it or disable the remote broker")
		}
	}

	if !cfg.Broker.Local.Enabled && !r.Enabled {
		add("both brokers are disabled, so nothing can ever start a run; set broker.local.enabled = true")
	}

	return problems
}

// validateBrokerURL requires TLS to anything that is not loopback.
//
// The Authorization header carries the machine token on every connection
// (§7.4). Over plaintext ws:// to a remote host that token is readable by
// anyone on the path, and a leaked machine token is a machine that builds
// whatever an attacker asks it to. Loopback is allowed unencrypted because the
// control plane is frequently run locally during development and there is no
// path to tap.
func validateBrokerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("broker.remote.url %q is not a URL: %w", raw, err)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := u.Hostname()
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		if host == "localhost" {
			return nil
		}
		return fmt.Errorf("broker.remote.url %q uses ws://, which sends the machine token in clear text; use wss:// (plain ws:// is allowed only to localhost)", raw)
	case "":
		return fmt.Errorf("broker.remote.url %q has no scheme; it must be wss://host/path", raw)
	default:
		return fmt.Errorf("broker.remote.url %q uses scheme %q; the remote broker is a WebSocket client and needs wss://", raw, u.Scheme)
	}
}

func validateRepos(cfg *Config) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if len(cfg.Repos) == 0 {
		add("no [repos.*] entries; a repo absent from this table cannot be built (§8), so this agent would reject every run — add at least one")
		return problems
	}

	for _, name := range sortedKeys(cfg.Repos) {
		repo := cfg.Repos[name]
		// The key becomes a mirror directory, mirrors/<key>.git (§12), so it
		// is constrained rather than trusted. It is also the exact string a
		// broker's `repo` field is matched against (INV-3 gate 1).
		if !nameKeyPattern.MatchString(name) {
			add("repos.%s: name must match %s — it becomes the mirror directory mirrors/<name>.git", name, nameKeyPattern)
		}
		if strings.TrimSpace(repo.URL) == "" {
			add("repos.%s: url is required", name)
		}
		if err := registry.ValidatePolicy(repo.AllowedRefs); err != nil {
			add("repos.%s: %v — patterns are globs where * does not cross / (INV-5)", name, err)
		}
		if len(repo.AllowedRefs) == 0 && !repo.RequireApprovalOtherwise {
			add("repos.%s: allowed_refs is empty and require_approval_otherwise is false, so no ref can ever run; list the refs you trust, or set require_approval_otherwise = true to gate them behind `latem approve`", name)
		}
		if repo.GitHub != "" && !githubSlugPattern.MatchString(repo.GitHub) {
			add("repos.%s: github = %q must be owner/name, the repository as GitHub names it", name, repo.GitHub)
		}
		problems = appendDuration(problems, "repos."+name+".approval_ttl", &repo.ApprovalTTL, DefaultApprovalTTL)
	}
	return problems
}

func validateRunners(cfg *Config) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if len(cfg.Runners) == 0 {
		add("no [[runner]] blocks; an agent with no execution slot can accept no work (§8)")
		return problems
	}

	seen := make(map[string]int, len(cfg.Runners))
	for i, r := range cfg.Runners {
		// The label used in messages: the name when there is one, the index
		// otherwise, because "runner[2]" is still enough to find the block.
		label := fmt.Sprintf("runner[%d]", i)
		if r.Name != "" {
			label = fmt.Sprintf("runner %q", r.Name)
		}

		switch {
		case r.Name == "":
			add("%s: name is required", label)
		case !nameKeyPattern.MatchString(r.Name):
			// The name becomes the Docker volume name latem-cache-<runner>-<cache>
			// (§14.1) and the host cache directory caches/<runner>/ (§14.2).
			add("%s: name must match %s — it becomes a Docker volume name and a cache directory", label, nameKeyPattern)
		default:
			if prev, dup := seen[r.Name]; dup {
				add("%s: duplicate name, already used by runner[%d]; names identify caches and runs, so two runners cannot share one", label, prev)
			}
			seen[r.Name] = i
		}

		if len(r.Labels) == 0 {
			add("%s: labels is required — a job's runs_on is matched against these, and a runner with none matches nothing (§10 gate 13)", label)
		}
		problems = append(problems, validateLabels(label+" labels", r.Labels)...)

		problems = append(problems, validateExecutor(r, label)...)
	}
	return problems
}

// validateExecutor checks the executor name, the concurrency ceilings, and the
// executor-specific block, filling that block's defaults.
func validateExecutor(r *Runner, label string) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if r.Concurrency == 0 {
		r.Concurrency = DefaultConcurrency
	}
	if r.Concurrency < 0 {
		add("%s: concurrency is %d; it must be at least 1", label, r.Concurrency)
	}

	switch r.Executor {
	case "":
		add("%s: executor is required; it must be %q, %q, or %q",
			label, ExecutorDocker, ExecutorTart, ExecutorNative)
		return problems
	case ExecutorDocker, ExecutorTart, ExecutorNative:
	default:
		add("%s: executor is %q; it must be %q, %q, or %q (§13)",
			label, r.Executor, ExecutorDocker, ExecutorTart, ExecutorNative)
		return problems
	}

	// A block for an executor this runner does not use is silently ignored at
	// runtime, which is how somebody spends an afternoon wondering why their
	// min_free_gb has no effect.
	for _, other := range []struct {
		name    string
		present bool
		owner   Executor
	}{
		{"docker", r.Docker != nil, ExecutorDocker},
		{"tart", r.Tart != nil, ExecutorTart},
		{"native", r.Native != nil, ExecutorNative},
	} {
		if other.present && r.Executor != other.owner {
			add("%s: has a [runner.%s] block but executor = %q, so those settings would be ignored; delete the block or change the executor",
				label, other.name, r.Executor)
		}
	}

	switch r.Executor {
	case ExecutorDocker:
		if r.Docker == nil {
			r.Docker = &DockerRunner{}
		}
		problems = append(problems, validateDocker(r.Docker, label)...)

	case ExecutorTart:
		// Apple's licence permits two guest VMs per host (§13.2). The third
		// clone fails with a licence error partway through a build, so the
		// ceiling is enforced here instead.
		if r.Concurrency > TartMaxConcurrency {
			add("%s: concurrency is %d; a tart runner is capped at %d by Apple's licence on guest VMs per host (§13.2)",
				label, r.Concurrency, TartMaxConcurrency)
		}
		if r.Tart == nil {
			r.Tart = &TartRunner{}
		}
		problems = append(problems, validateTart(r.Tart, label)...)

	case ExecutorNative:
		// Parallel Xcode builds contend on DerivedData and the keychain, and
		// produce failures that look like code bugs (§13.3).
		if r.Concurrency != 1 {
			add("%s: concurrency is %d; a native runner must be 1 — parallel builds on one host contend on DerivedData and the keychain, and fail in ways that look like code bugs (§13.3)",
				label, r.Concurrency)
		}
		if r.Native == nil {
			r.Native = &NativeRunner{}
		}
		problems = append(problems, validateNative(r.Native, label)...)
	}

	return problems
}

func validateDocker(d *DockerRunner, label string) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if d.Network == "" {
		d.Network = DefaultDockerNetwork
	}
	if d.CPUs < 0 {
		add("%s: docker.cpus is %v; it must be positive, or absent for unlimited", label, d.CPUs)
	}
	if d.Memory != "" {
		if _, err := parseMemory(d.Memory); err != nil {
			add("%s: docker.memory %q: %v", label, d.Memory, err)
		}
	}
	problems = append(problems, checkMinFree(&d.MinFreeGB, DefaultDockerMinFreeGB, label, "docker")...)
	return problems
}

func validateTart(t *TartRunner, label string) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if t.BaseImage == "" {
		add("%s: tart.base_image is required; build one with the script in examples/ (§13.2)", label)
	}
	if t.SSHUser == "" {
		t.SSHUser = DefaultTartSSHUser
	}
	if t.CPUs < 0 {
		add("%s: tart.cpus is %d; it must be positive, or absent to keep the image's setting", label, t.CPUs)
	}
	if t.MemoryGB < 0 {
		add("%s: tart.memory_gb is %d; it must be positive, or absent to keep the image's setting", label, t.MemoryGB)
	}
	problems = appendDuration(problems, label+": tart.boot_timeout", &t.BootTimeout, DefaultBootTimeout)

	switch t.DerivedData {
	case "":
		// "vm" until WS-09's measurement lands (§13.2). The VirtioFS share can
		// be pathologically slow for DerivedData; the VM's own disk cannot.
		t.DerivedData = DerivedDataVM
	case DerivedDataVM, DerivedDataShared:
	default:
		add("%s: tart.derived_data is %q; it must be %q or %q (§13.2)",
			label, t.DerivedData, DerivedDataVM, DerivedDataShared)
	}

	problems = append(problems, checkMinFree(&t.MinFreeGB, DefaultTartMinFreeGB, label, "tart")...)
	return problems
}

func validateNative(n *NativeRunner, label string) []error {
	return checkMinFree(&n.MinFreeGB, DefaultNativeMinFreeGB, label, "native")
}

// checkMinFree applies the §13 default and rejects a negative floor.
//
// Zero is read as absent rather than as "no floor". Turning the floor off is
// not something a config should be able to do by accident, and §14.4 has no
// notion of a runner that accepts work regardless of free disk — the floor is
// what stops a machine taking an iOS build it cannot finish.
func checkMinFree(v *float64, def float64, label, block string) []error {
	if *v < 0 {
		return []error{fmt.Errorf("%s: %s.min_free_gb is %v; it must be positive (the default is %v)", label, block, *v, def)}
	}
	if *v == 0 {
		*v = def
	}
	return nil
}

func validateSecrets(cfg *Config) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	switch cfg.Secrets.Provider {
	case "":
		cfg.Secrets.Provider = SecretsProviderChain
	case SecretsProviderChain:
	default:
		add("secrets.provider is %q; the only provider is %q — environment, then secrets.env, then the keychain, then pass, then a per-secret command, first hit wins (§15)",
			cfg.Secrets.Provider, SecretsProviderChain)
	}

	for _, name := range sortedKeys(cfg.Secrets.Commands) {
		if !envNamePattern.MatchString(name) {
			add("secrets.commands.%s: name must match %s — it is the name a job declares in its secrets: block, and it becomes an environment variable", name, envNamePattern)
		}
		if strings.TrimSpace(cfg.Secrets.Commands[name]) == "" {
			add("secrets.commands.%s: command is empty; remove the entry or give it a command", name)
		}
	}
	return problems
}

// validateFilesystem is the part of §8 that has to stat something.
//
// It is skipped by [Parse], which validates content with no file behind it, so
// `latem doctor` can check a candidate config without the candidate's modes
// standing in for the real ones.
func validateFilesystem(cfg *Config) []error {
	var problems []error

	// The config directory's own mode is checked by [Load] before it reads the
	// file, since a directory the agent cannot enter breaks the read first.
	if err := checkFileMode(cfg.Paths.File, "config.toml", true); err != nil {
		problems = append(problems, err)
	}
	if err := checkFileMode(cfg.Paths.SecretsEnv, "secrets.env", false); err != nil {
		problems = append(problems, err)
	}
	// The token file only has to exist when something will read it. Requiring
	// it unconditionally would stop an unregistered machine from running local
	// builds, which is a supported mode (§7.3).
	if cfg.Broker.Remote.Enabled {
		if err := checkFileMode(cfg.Paths.TokenFile, "broker.remote.token_file", true); err != nil {
			problems = append(problems, err)
		}
	}

	if err := checkSameFilesystem(cfg.Paths.Mirrors, cfg.Paths.Work); err != nil {
		problems = append(problems, err)
	}

	return problems
}

// checkDirMode requires the config directory to be reachable by its owner and
// nobody else.
func checkDirMode(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config directory %s does not exist; create it with `mkdir -p %s && chmod 0700 %s`", dir, dir, dir)
		}
		return fmt.Errorf("config directory %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("config directory %s is not a directory", dir)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		return fmt.Errorf("config directory %s is mode %04o; it holds secrets.env and the machine token and must not be reachable by group or other — `chmod 0700 %s`", dir, perm, dir)
	}
	if perm&0o700 != 0o700 {
		return fmt.Errorf("config directory %s is mode %04o; the agent cannot enter and write its own state directory — `chmod 0700 %s`", dir, perm, dir)
	}
	return nil
}

// checkFileMode requires a file to be readable by its owner alone.
//
// The test is "no bits for group or other" rather than "exactly 0600", so a
// token file at 0400 — which is stricter, and a thing careful people do —
// passes. required distinguishes a file that must be there from one that is
// optional but must be safe if present.
func checkFileMode(path, what string, required bool) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if required {
				return fmt.Errorf("%s (%s) does not exist", what, path)
			}
			return nil
		}
		return fmt.Errorf("%s (%s): %w", what, path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s (%s) is mode %04o; it must not be readable by group or other — `chmod 0600 %s`", what, path, perm, path)
	}
	return nil
}

// checkSameFilesystem enforces §12's hardlink requirement.
//
// `git clone --local` hardlinks the mirror's object store, which is what makes
// a per-run clone nearly free. Across filesystems git silently copies instead,
// and every run gets slower in a way nobody attributes to the right cause. The
// check is skipped when either directory is absent, because on a fresh install
// neither exists yet and they will be created adjacent by default.
func checkSameFilesystem(mirrors, work string) error {
	same, ok, err := sameFilesystem(mirrors, work)
	if err != nil || !ok || same {
		return nil
	}
	return fmt.Errorf("mirrors (%s) and work_dir (%s) are on different filesystems; `git clone --local` then copies the whole object store instead of hardlinking it and every run gets slower for no visible reason (§12) — put work_dir on the mirrors' filesystem, or remove agent.work_dir so it derives from the config directory",
		mirrors, work)
}

func validateLabels(what string, labels []string) []error {
	var problems []error
	seen := make(map[string]bool, len(labels))
	for _, l := range labels {
		if !labelPattern.MatchString(l) {
			problems = append(problems, fmt.Errorf("%s: label %q must match %s — jobs declare runs_on with this class, so a label outside it can never match one",
				what, l, labelPattern))
			continue
		}
		if seen[l] {
			problems = append(problems, fmt.Errorf("%s: label %q appears twice", what, l))
		}
		seen[l] = true
	}
	return problems
}

// appendDuration rejects a negative duration and defaults a zero one.
func appendDuration(problems []error, name string, d *protocol.Duration, def time.Duration) []error {
	if *d < 0 {
		return append(problems, fmt.Errorf("%s is %s; it must be positive", name, d))
	}
	if *d == 0 {
		*d = protocol.Duration(def)
	}
	return problems
}

// memoryPattern matches the Docker size strings §8 uses: "8g", "512m", or a
// bare byte count.
var memoryPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kmgtKMGT]?)[iI]?[bB]?$`)

var memoryUnits = map[string]int64{
	"":  1,
	"k": 1 << 10,
	"m": 1 << 20,
	"g": 1 << 30,
	"t": 1 << 40,
}

// MemoryBytes is the runner's memory limit in bytes, or 0 when unlimited.
//
// The value is parsed at load rather than handed to Docker as a string so a
// typo like "8gb " fails here instead of at container create, which happens
// after the workspace is cloned and the caches are locked.
func (d *DockerRunner) MemoryBytes() int64 {
	if d == nil || d.Memory == "" {
		return 0
	}
	n, err := parseMemory(d.Memory)
	if err != nil {
		return 0
	}
	return n
}

func parseMemory(s string) (int64, error) {
	m := memoryPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, errors.New(`not a size; write a number with an optional k, m, g, or t suffix, as in "8g"`)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("not a size: %w", err)
	}
	scaled := n * float64(memoryUnits[strings.ToLower(m[2])])
	if scaled <= 0 {
		return 0, errors.New("is zero; omit the key for no limit")
	}
	return int64(scaled), nil
}
