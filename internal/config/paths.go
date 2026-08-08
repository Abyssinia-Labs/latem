package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Paths is every location the agent owns, derived from one config directory.
//
// It exists as a single struct because §8 makes one promise about relocation:
// --config-dir or LATEM_CONFIG_DIR moves the lock, the socket, the sqlite
// database, the work directory, and the caches *together*. A second agent on
// one machine that shares any one of them is not a second agent, it is two
// processes corrupting one state directory. Deriving them all from Dir is what
// makes "together" structural rather than a thing five packages remember.
type Paths struct {
	// Dir is the config directory itself, mode 0700.
	Dir string
	// File is <Dir>/config.toml, mode 0600.
	File string
	// Lock is the flock the agent holds for its whole life. Exactly one agent
	// per config dir (§6 step 2).
	Lock string
	// Socket is the local broker's Unix socket, mode 0600 (§7.3).
	Socket string
	// DB is the sqlite state database (§22).
	DB string
	// Work holds one self-contained clone per run, deleted after collecting
	// (§12).
	Work string
	// Caches holds host cache directories for tart and native runners (§14.2).
	// Docker caches are named volumes and do not live here.
	Caches string
	// Mirrors holds one bare mirror per repo. It has to be on the same
	// filesystem as Work or `git clone --local` stops hardlinking (§12).
	Mirrors string
	// Logs holds <run-id>.log (§16).
	Logs string
	// Artifacts holds <run-id>/ (§17).
	Artifacts string
	// SecretsEnv is the optional KEY=value file, mode 0600 enforced (§15).
	SecretsEnv string
	// TokenFile holds the machine token, mode 0600 (§7.4).
	TokenFile string
}

// Base filenames under the config directory.
const (
	lockName       = "agent.lock"
	socketName     = "agent.sock"
	dbName         = "state.db"
	workName       = "work"
	cachesName     = "caches"
	mirrorsName    = "mirrors"
	logsName       = "logs"
	artifactsName  = "artifacts"
	secretsEnvName = "secrets.env"
	tokenName      = "agent.key"
)

// maxSocketPathLen is the shorter of the two platform limits on a Unix socket
// path — 104 on macOS, 108 on Linux — minus the NUL.
//
// This is checked at load because it is the failure mode --config-dir invites.
// A relocated config directory a few levels deep produces a socket path that
// bind(2) rejects with EINVAL, and "invalid argument" from a socket call is
// not a message anyone traces back to the length of a directory name.
const maxSocketPathLen = 103

// RunnerCacheDir is where a tart or native runner's caches live (§14.2). The
// runner name is in the path, which is why it is constrained to a character
// class that cannot traverse a directory.
func (p Paths) RunnerCacheDir(runner string) string {
	return filepath.Join(p.Caches, runner)
}

// MirrorDir is the bare mirror for a repo key (§12).
func (p Paths) MirrorDir(repo string) string {
	return filepath.Join(p.Mirrors, repo+".git")
}

// derivePaths fills every path from the config directory. Overrides from the
// config file are applied afterwards, by [applyPathOverrides].
func derivePaths(dir string) Paths {
	return Paths{
		Dir:        dir,
		File:       filepath.Join(dir, FileName),
		Lock:       filepath.Join(dir, lockName),
		Socket:     filepath.Join(dir, socketName),
		DB:         filepath.Join(dir, dbName),
		Work:       filepath.Join(dir, workName),
		Caches:     filepath.Join(dir, cachesName),
		Mirrors:    filepath.Join(dir, mirrorsName),
		Logs:       filepath.Join(dir, logsName),
		Artifacts:  filepath.Join(dir, artifactsName),
		SecretsEnv: filepath.Join(dir, secretsEnvName),
		TokenFile:  filepath.Join(dir, tokenName),
	}
}

// Options controls where the config comes from and what the loader is allowed
// to consult. The zero value is what an agent uses: ~/.latem, the real
// environment, the real resolver.
type Options struct {
	// Dir is --config-dir. It wins over LATEM_CONFIG_DIR, which wins over
	// ~/.latem. A flag beating an environment variable is the ordering people
	// expect when they are debugging a second agent by hand.
	Dir string
	// Getenv defaults to [os.Getenv].
	Getenv func(string) string
	// Home defaults to [os.UserHomeDir] and is what "~" expands to.
	Home string
	// LookupIP defaults to [net.LookupIP] and resolves a non-literal
	// metrics_addr host. Injectable so the INV-1 loopback rule can be tested
	// without depending on what this machine believes "localhost" means.
	LookupIP func(host string) ([]net.IP, error)

	// skipFilesystem suppresses the checks that stat something: directory and
	// file modes, and the mirrors/work same-filesystem rule. Set by [Parse],
	// which validates content with no file behind it.
	skipFilesystem bool
}

// resolve fills the defaults and pins the config directory, so a later reload
// reads the same file even if the environment changed underneath it.
func (o Options) resolve() (Options, error) {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.LookupIP == nil {
		o.LookupIP = net.LookupIP
	}
	if o.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return o, fmt.Errorf("config: cannot determine the home directory: %w", err)
		}
		o.Home = home
	}

	dir := o.Dir
	if dir == "" {
		dir = o.Getenv(EnvConfigDir)
	}
	if dir == "" {
		dir = filepath.Join(o.Home, DirName)
	}
	expanded, err := expandPath(dir, o.Home)
	if err != nil {
		return o, fmt.Errorf("config: config directory: %w", err)
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return o, fmt.Errorf("config: config directory %q: %w", expanded, err)
	}
	o.Dir = abs
	return o, nil
}

// relocated reports whether the config directory is somewhere other than the
// default ~/.latem.
func (o Options) relocated() bool { return o.Dir != filepath.Join(o.Home, DirName) }

func (o Options) defaultDir() string { return filepath.Join(o.Home, DirName) }

func (o Options) configFile() string { return filepath.Join(o.Dir, FileName) }

// Load reads and validates <config-dir>/config.toml.
//
// This is step 1 of §6 and it fails fast: an agent that starts on a config it
// could not fully validate is an agent that will fail during someone's build
// instead.
func Load(opts Options) (*Config, error) {
	resolved, err := opts.resolve()
	if err != nil {
		return nil, err
	}

	// The directory mode is checked before the read rather than alongside the
	// other filesystem rules, because a config directory the agent cannot
	// enter makes the read itself fail with a bare "permission denied" that
	// says nothing about which directory or what to do about it.
	var earlier []error
	if err := checkDirMode(resolved.Dir); err != nil {
		earlier = append(earlier, err)
	}

	path := resolved.configFile()
	// #nosec G304 -- path is <config-dir>/config.toml, where the directory came
	// from --config-dir, LATEM_CONFIG_DIR, or $HOME. All three are the
	// operator's own input to their own agent; none crosses a trust boundary.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &Error{Path: path, Problems: append(earlier,
				errors.New("no config file here — run `latem register --url U --token T` to create one, or copy the example from RUNNER_SPEC §8"),
			)}
		}
		return nil, &Error{Path: path, Problems: append(earlier, err)}
	}

	cfg, err := decodeAndValidate(data, resolved, earlier)
	if err != nil {
		return nil, err
	}
	cfg.source = &resolved
	return cfg, nil
}

// expandPath expands a leading "~" and nothing else.
//
// Environment variables are deliberately not expanded. A config value that
// changes meaning with the environment the agent happened to inherit is a
// property nobody wants in the file that decides which repositories may be
// built, and $HOME under a LaunchAgent is frequently not what the operator
// assumed when they wrote the line.
func expandPath(p, home string) (string, error) {
	switch {
	case p == "":
		return "", nil
	case p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:]), nil
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%q: ~user expansion is not supported; write the absolute path", p)
	}
	return p, nil
}

// resolvePathValue expands a config path value against the home directory, and
// resolves a relative one against the config directory rather than the process
// working directory.
//
// Resolving against the config dir is the choice that keeps §8's relocation
// promise: `work_dir = "work"` follows --config-dir, while resolving against
// the cwd would make the agent's state depend on where it was launched from.
func resolvePathValue(p, home, dir string) (string, error) {
	expanded, err := expandPath(p, home)
	if err != nil {
		return "", err
	}
	if expanded == "" {
		return "", nil
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(dir, expanded)
	}
	return filepath.Clean(expanded), nil
}

// applyPathOverrides folds the three config keys that can move a derived path
// into [Paths], and reports the ways that can go wrong.
//
// The relocation guard is the interesting part. §8 promises that relocating
// the config directory relocates the state with it, and the way that promise
// breaks in practice is mundane: someone copies their config.toml — including
// the `work_dir = "~/.latem/work"` the spec's own example shows — and points a
// second agent at it with --config-dir. Nothing complains, and two agents
// share a work directory, a socket, and a token. Naming that at load is worth
// far more than the flexibility of allowing it.
func applyPathOverrides(cfg *Config, opts Options) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	override := func(key, value string, dst *string) {
		if value == "" {
			return
		}
		resolved, err := resolvePathValue(value, opts.Home, opts.Dir)
		if err != nil {
			add("%s: %v", key, err)
			return
		}
		if opts.relocated() && within(resolved, opts.defaultDir()) {
			add("%s points at %s, inside the default config directory, but this agent's config directory is %s; "+
				"two agents sharing state corrupt each other's runs — remove %s so it derives from the config directory, or point it inside %s",
				key, resolved, opts.Dir, key, opts.Dir)
			return
		}
		*dst = resolved
	}

	override("agent.work_dir", cfg.Agent.WorkDir, &cfg.Paths.Work)
	override("broker.local.socket", cfg.Broker.Local.Socket, &cfg.Paths.Socket)
	override("broker.remote.token_file", cfg.Broker.Remote.TokenFile, &cfg.Paths.TokenFile)

	// Write the effective values back so a caller reading cfg.Agent.WorkDir
	// and a caller reading cfg.Paths.Work cannot disagree.
	cfg.Agent.WorkDir = cfg.Paths.Work
	cfg.Broker.Local.Socket = cfg.Paths.Socket
	cfg.Broker.Remote.TokenFile = cfg.Paths.TokenFile

	if n := len(cfg.Paths.Socket); n > maxSocketPathLen {
		add("broker.local.socket is %d bytes (%s); the kernel caps a Unix socket path at %d and bind fails with a bare \"invalid argument\" — use a shorter config directory",
			n, cfg.Paths.Socket, maxSocketPathLen)
	}

	return problems
}

// within reports whether path is dir or sits underneath it. It is lexical on
// purpose: it runs before any of these directories necessarily exist, so it
// cannot follow symlinks or stat anything.
func within(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
