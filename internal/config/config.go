// Package config loads and validates ~/.latem/config.toml.
//
// Every rule here is fatal at load, and RUNNER_SPEC §8 gives the reason: a
// runner that starts with a broken config does not fail at startup, it fails
// forty minutes into a build, with an error that points at the build rather
// than at the file. The cheapest possible moment to reject `concurrency = 4`
// on a Tart runner is before the first VM boots.
//
// Problems are collected rather than returned one at a time, the way
// internal/registry collects them. Someone repairing a config should see
// everything wrong with it in one pass instead of discovering the next fault
// after each restart.
//
// Three validations here are load-bearing for the security invariants rather
// than merely for operator convenience:
//
//   - metrics_addr must resolve to loopback (INV-1, §20). The parser is the
//     only place this can be enforced cheaply, and it runs before anything
//     binds a socket.
//   - the config directory, the machine token, and secrets.env must not be
//     reachable by group or other. A world-readable ~/.latem holding a machine
//     token and secrets.env is the whole threat model failing quietly (§8).
//   - runner names and repo keys become filesystem paths and Docker volume
//     names, so they are constrained to a character class that cannot traverse
//     a directory.
package config

import (
	"fmt"
	"net"
	"regexp"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// SchemaVersion is the only value of the top-level schema_version key this
// agent understands. A mismatch is fatal with a migration hint (§8).
const SchemaVersion = 1

// Filesystem names under the config directory. They are constants rather than
// literals scattered through the tree because relocating the config directory
// has to relocate all of them together (§8), and that guarantee is only as
// good as the number of places that spell these out.
const (
	// DirName is the config directory under $HOME when nothing relocates it.
	DirName = ".latem"
	// FileName is the config file inside the config directory.
	FileName = "config.toml"
	// EnvConfigDir relocates the config directory, and with it the lock, the
	// socket, the sqlite database, the work directory, and the caches.
	EnvConfigDir = "LATEM_CONFIG_DIR"
)

// Defaults from RUNNER_SPEC §8, §12, §13, and §20. Absent keys take these
// values; the file in §8 is an illustration of the schema, not a required
// minimum, so an agent must run from a config that sets almost nothing.
const (
	DefaultShutdownGrace = 30 * time.Minute
	DefaultLogRetention  = 14 * 24 * time.Hour
	DefaultQueueTTL      = 72 * time.Hour
	DefaultApprovalTTL   = 24 * time.Hour
	DefaultMetricsAddr   = "127.0.0.1:9838"
	DefaultDockerNetwork = "bridge"
	DefaultTartSSHUser   = "admin"
	DefaultBootTimeout   = 3 * time.Minute
	DefaultConcurrency   = 1
)

// Per-executor disk floors from §13 and §14.4. Draft 01's single global 20 GB
// was sized for a container host and would admit an iOS build onto a machine
// that cannot hold one.
//
// The Tart floor is 200 rather than draft 02's 150 (amendment A31). That figure
// came from an estimate of "around 90 GB" for an Xcode image; measured, the
// image is 68.8 GB compressed and **140 GB provisioned**, so 150 left under
// 10 GB of headroom above a single image — before the clone diverges and
// DerivedData lands on top of it.
const (
	DefaultDockerMinFreeGB float64 = 40
	DefaultNativeMinFreeGB float64 = 60
	DefaultTartMinFreeGB   float64 = 200
)

// TartMaxConcurrency is Apple's licence ceiling: two guest VMs per host
// (§13.2). It is enforced at load rather than discovered when the third `tart
// clone` produces a licence error partway through a release build.
const TartMaxConcurrency = 2

// Executor names one of the three implementations in §13.
type Executor string

// The closed set of executors. Anything else is a typo, and a typo that
// reached runtime would present as "no runner carries these labels".
const (
	ExecutorDocker Executor = "docker"
	ExecutorTart   Executor = "tart"
	ExecutorNative Executor = "native"
)

// RemoteLogs controls whether log frames ship live or only on failure (§7.4).
type RemoteLogs string

// The two declared values. on_failure exists for verbose Gradle output; it
// trades live viewing for bandwidth.
const (
	RemoteLogsAlways    RemoteLogs = "always"
	RemoteLogsOnFailure RemoteLogs = "on_failure"
)

// DerivedData selects where Xcode's DerivedData lives for a Tart runner
// (§13.2).
type DerivedData string

// The two declared values.
const (
	// DerivedDataVM keeps DerivedData on the VM's own disk.
	DerivedDataVM DerivedData = "vm"
	// DerivedDataShared shares it from the host over VirtioFS.
	DerivedDataShared DerivedData = "shared"
)

// SecretsProviderChain is the only provider §15 defines: environment, then
// secrets.env, then the macOS keychain, then pass/gopass, then a per-secret
// command, first hit wins.
const SecretsProviderChain = "chain"

var (
	// labelPattern matches internal/registry's own label rule on purpose.
	// Runner labels are compared against a job's runs_on, which registry
	// validates with this class, so a runner labelled "Xcode-16" could never
	// match any job — a failure that presents as E_NO_RUNNER and sends the
	// operator looking at the wrong file.
	labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// nameKeyPattern shapes runner names and repo keys. Both become paths on
	// disk — caches/<runner>/, mirrors/<repo>.git — and a runner name is also
	// embedded in a Docker volume name (§14.1). Excluding every separator, and
	// requiring an alphanumeric first character, is what stops a config from
	// naming a directory outside the config dir.
	nameKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// machineNamePattern is display-only, so it is permissive about spaces and
	// strict about everything that would mangle a terminal or a log line.
	machineNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	// machineIDPattern matches the "mch_01H..." shape §8 shows. The id is
	// assigned by the control plane and echoed in heartbeats; constraining it
	// keeps a hostile registration response from becoming a path or a log
	// injection later.
	machineIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	// envNamePattern shapes [secrets.commands] keys, which become the names of
	// secrets a job declares. registry validates those with the C identifier
	// rules because they end up as environment variables.
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Config is a parsed, validated, and defaulted config.toml.
//
// Field types are chosen so WS-10 can adapt them to registry.Policy without a
// conversion table: AllowedRefs stays a raw pattern list (registry.ExpandPolicy
// owns the expansion), RequireApprovalOtherwise keeps its name and its false
// default, and disk floors are float64 to match registry.Policy and
// protocol.MachineHealth.
type Config struct {
	SchemaVersion int              `toml:"schema_version"`
	Machine       Machine          `toml:"machine"`
	Agent         Agent            `toml:"agent"`
	Broker        Broker           `toml:"broker"`
	Repos         map[string]*Repo `toml:"repos"`
	Runners       []*Runner        `toml:"runner"`
	Secrets       Secrets          `toml:"secrets"`

	// Paths is derived from the config directory, never decoded. It is the
	// single answer to "where does this agent keep its state", so relocating
	// the config dir relocates everything at once (§8).
	Paths Paths `toml:"-"`

	// source records how this config was loaded so [Reloader] can reload it
	// from the same place with the same options. Nil for a [Parse] result,
	// which has no file behind it.
	source *Options
}

// Machine is display and identity, not behaviour.
type Machine struct {
	// Name is shown in a UI and nothing else reads it.
	Name string `toml:"name"`
	// ID is assigned by `latem register` and stable thereafter. It is empty on
	// a machine that has never registered, which is a supported state: the
	// local broker works without a control plane (§7.3).
	ID string `toml:"id"`
	// Labels advertise the machine in heartbeats. Job routing matches against
	// per-runner labels, not these (§10 gate 13).
	Labels []string `toml:"labels"`
}

// Agent holds the daemon-wide knobs from §8.
type Agent struct {
	// ShutdownGrace is how long in-flight runs may continue after SIGTERM
	// before their process groups are signalled (§6). The default is generous
	// because an iOS build is long.
	ShutdownGrace protocol.Duration `toml:"shutdown_grace"`
	// WorkDir holds one clone per run. Empty derives it from the config dir,
	// which is what makes --config-dir relocate it (§8).
	WorkDir      string            `toml:"work_dir"`
	LogRetention protocol.Duration `toml:"log_retention"`
	QueueTTL     protocol.Duration `toml:"queue_ttl"`
	// AutoUpdate is off by default. Self-update re-execs the agent, and doing
	// that to somebody's build machine without being asked is not a default
	// anyone should have to discover (§20).
	//
	// When on: the agent checks the release feed daily, verifies a minisign
	// signature over the release's checksums manifest against the key embedded
	// in this binary, and replaces itself only while no run is in flight.
	//
	// **A build with no embedded signing key refuses the value at load**
	// rather than accepting it and declining every release in a log nobody
	// reads. See validateAgent.
	AutoUpdate bool       `toml:"auto_update"`
	RemoteLogs RemoteLogs `toml:"remote_logs"`
	// MetricsAddr must be a loopback address or empty (INV-1). Absent means
	// [DefaultMetricsAddr]; an explicit "" disables metrics. The distinction
	// is deliberate — deleting the line is how you get the documented default,
	// not how you turn metrics off.
	MetricsAddr string `toml:"metrics_addr"`
}

// Broker groups the two transports. The agent contains no branch on which is
// in use; both satisfy the §7 interface.
type Broker struct {
	Local  LocalBroker  `toml:"local"`
	Remote RemoteBroker `toml:"remote"`
}

// LocalBroker is the Unix socket in §7.3.
type LocalBroker struct {
	// Enabled defaults to true. §6 starts the local broker unconditionally,
	// and an agent without it cannot be reached by `latem run` at all.
	Enabled bool `toml:"enabled"`
	// Socket empty derives from the config dir.
	Socket string `toml:"socket"`
}

// RemoteBroker is the WebSocket client in §7.4.
type RemoteBroker struct {
	Enabled bool   `toml:"enabled"`
	URL     string `toml:"url"`
	// TokenFile holds the machine token. It is a path, never an inline value:
	// the token would otherwise sit in a file that gets pasted into issues.
	TokenFile string `toml:"token_file"`
}

// Repo is one entry in the allowlist. A repo absent from this table cannot be
// built, which is what makes the config an allowlist rather than a hint (§8,
// gate 1).
type Repo struct {
	URL string `toml:"url"`
	// AllowedRefs are raw patterns as written. Expansion to fully-qualified
	// names is registry.ExpandPolicy's job, so "main" means refs/heads/main
	// and never refs/tags/main (INV-5).
	AllowedRefs []string `toml:"allowed_refs"`
	// RequireApprovalOtherwise sends a non-matching ref to awaiting_approval.
	// The false default is the safe one: a non-matching ref is rejected
	// outright rather than queued for someone to rubber-stamp.
	RequireApprovalOtherwise bool              `toml:"require_approval_otherwise"`
	ApprovalTTL              protocol.Duration `toml:"approval_ttl"`
}

// Runner is one execution slot. One agent hosts several; "two roles on one
// box" means two [[runner]] blocks with different labels, not two agents (§8).
type Runner struct {
	Name     string   `toml:"name"`
	Executor Executor `toml:"executor"`
	Labels   []string `toml:"labels"`
	// Concurrency is how many runs this slot may execute at once. The
	// executor-specific ceilings in §13 are enforced at load.
	Concurrency int `toml:"concurrency"`

	// Exactly one of these matches Executor. The others must be absent: a
	// [runner.tart] block on a docker runner is silently ignored at runtime,
	// which is the kind of mistake that costs an afternoon.
	Docker *DockerRunner `toml:"docker"`
	Tart   *TartRunner   `toml:"tart"`
	Native *NativeRunner `toml:"native"`
}

// DockerRunner is §13.1.
type DockerRunner struct {
	// DefaultImage applies to jobs that do not name their own image.
	DefaultImage string `toml:"default_image"`
	// GPU adds device requests for all GPUs. Preflight verifies the NVIDIA
	// toolkit at startup so a GPU job fails the runner rather than the build.
	GPU     bool    `toml:"gpu"`
	Network string  `toml:"network"`
	CPUs    float64 `toml:"cpus"`
	// Memory is a Docker size string such as "8g". Empty means unlimited.
	Memory string `toml:"memory"`
	// MinFreeGB refuses new runs below this floor (§14.4). Zero takes
	// [DefaultDockerMinFreeGB].
	MinFreeGB float64 `toml:"min_free_gb"`
	// RequirePinnedImages refuses a `uses: docker://` step whose image is
	// named by tag rather than by digest (A44). Off by default: a tag is
	// allowed and the digest it resolved to is written to the run log. It
	// does not apply to default_image, which the operator chose here.
	RequirePinnedImages bool `toml:"require_pinned_images"`
}

// TartRunner is §13.2.
type TartRunner struct {
	BaseImage   string            `toml:"base_image"`
	CPUs        int               `toml:"cpus"`
	MemoryGB    int               `toml:"memory_gb"`
	SSHUser     string            `toml:"ssh_user"`
	BootTimeout protocol.Duration `toml:"boot_timeout"`
	// MinFreeGB defaults to 150: a base image carrying Xcode is around 90 GB
	// and a running VM diverges from it.
	MinFreeGB float64 `toml:"min_free_gb"`
	// DerivedData exists so §13.2's open measurement is a config value rather
	// than a rewrite. It defaults to "vm" until WS-09 reports: DerivedData on
	// the VM's own disk cannot be pathologically slow, while the VirtioFS
	// share can be, and a default that might lose badly is worse than one that
	// merely might not win.
	DerivedData DerivedData `toml:"derived_data"`
}

// NativeRunner is §13.3 — the default macOS executor since amendment A31, and
// the one with no isolation of any kind.
//
// Those two facts belong in one sentence. It is the default because on a Mac
// that already has Xcode it costs nothing to run, and it is correct only while
// the machine builds refs the operator's own team controls; §13.0 states the
// criterion and §13.2 is the upgrade when it stops holding.
//
// The block exists so the disk floor is settable; everything else about the
// native executor is determined by the host.
type NativeRunner struct {
	// MinFreeGB defaults to [DefaultNativeMinFreeGB].
	MinFreeGB float64 `toml:"min_free_gb"`
}

// Secrets configures the §15 resolution chain. Values never appear here; only
// the means of obtaining them.
type Secrets struct {
	Provider string `toml:"provider"`
	// Commands is a per-secret shell command, the last link in the chain.
	Commands map[string]string `toml:"commands"`
}

// MinFreeGB is the effective disk floor for this runner, with the §13 default
// for its executor applied.
//
// It reads through the executor-specific block rather than a flattened field
// so there is one place the value can come from, and callers cannot pick up a
// docker floor for a tart runner by reaching into the wrong struct.
func (r *Runner) MinFreeGB() float64 {
	switch r.Executor {
	case ExecutorDocker:
		if r.Docker != nil && r.Docker.MinFreeGB > 0 {
			return r.Docker.MinFreeGB
		}
		return DefaultDockerMinFreeGB
	case ExecutorTart:
		if r.Tart != nil && r.Tart.MinFreeGB > 0 {
			return r.Tart.MinFreeGB
		}
		return DefaultTartMinFreeGB
	case ExecutorNative:
		if r.Native != nil && r.Native.MinFreeGB > 0 {
			return r.Native.MinFreeGB
		}
		return DefaultNativeMinFreeGB
	}
	return 0
}

// Runner returns the runner with the given name.
func (c *Config) Runner(name string) (*Runner, bool) {
	for _, r := range c.Runners {
		if r.Name == name {
			return r, true
		}
	}
	return nil, false
}

// WeakestMinFreeGB is the most permissive disk floor across all runners.
//
// It is what gate 5 of §10 checks, before a runner has been selected: a
// request that cannot satisfy even the loosest floor on the machine is
// rejected without a fetch. The selected runner's own, possibly much larger,
// floor is re-checked at gate 17.
func (c *Config) WeakestMinFreeGB() float64 {
	weakest := 0.0
	for i, r := range c.Runners {
		f := r.MinFreeGB()
		if i == 0 || f < weakest {
			weakest = f
		}
	}
	return weakest
}

// MetricsEnabled reports whether the agent should serve metrics.
func (c *Config) MetricsEnabled() bool { return c.Agent.MetricsAddr != "" }

// Parse validates a config that is not on disk.
//
// It performs every check except the ones needing the filesystem — directory
// and file modes, and the mirrors/work_dir same-filesystem rule — so it is the
// right entry point for `latem doctor` checking a candidate file and for
// tests. [Load] is what an agent uses.
func Parse(data []byte, opts Options) (*Config, error) {
	opts, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	opts.skipFilesystem = true
	return decodeAndValidate(data, opts, nil)
}

// decodeAndValidate is the one pipeline both Parse and Load run, so a config
// never has two different meanings depending on how it was read.
//
// earlier carries problems found before the file could be decoded — the
// directory mode, which has to be checked before the read that it can make
// fail — so they join the same report rather than pre-empting it.
func decodeAndValidate(data []byte, opts Options, earlier []error) (*Config, error) {
	cfg := &Config{}
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		// A decode error is structural — a duplicate key, a string where a
		// number belongs — and nothing downstream can be trusted, so this is
		// the one place that stops at the first problem.
		return nil, &Error{Path: opts.configFile(), Problems: append(earlier, err)}
	}

	problems := append([]error(nil), earlier...)
	problems = append(problems, unknownKeys(&md)...)
	problems = append(problems, validate(cfg, &md, opts)...)
	if len(problems) > 0 {
		return nil, &Error{Path: opts.configFile(), Problems: problems}
	}
	return cfg, nil
}

// unknownKeys turns strict decoding into an error list.
//
// A typo in a config file must not be a warning. `concurency = 4` on a tart
// runner would otherwise leave the licence ceiling unenforced and the value
// silently at its default, and the operator would have no way to tell from the
// outside which of the two happened.
func unknownKeys(md *toml.MetaData) []error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	problems := make([]error, 0, len(undecoded))
	for _, k := range undecoded {
		problems = append(problems, fmt.Errorf("unknown key %q — check the spelling against RUNNER_SPEC §8", k.String()))
	}
	return problems
}

// lookupIPFunc is the resolver seam used for metrics_addr validation. It is a
// field on [Options] so a test can assert the loopback rule without depending
// on what the machine's resolver says about "localhost".
type lookupIPFunc func(host string) ([]net.IP, error)
