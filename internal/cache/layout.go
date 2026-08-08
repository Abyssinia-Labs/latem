package cache

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Executor kinds, as they appear in the agent's config. A runner's kind decides
// what backs its caches: Docker named volumes, or host directories shared into
// a VM with `tart run --dir` (RUNNER_SPEC §14.1, §14.2).
const (
	ExecutorDocker = "docker"
	ExecutorTart   = "tart"
	ExecutorNative = "native"
)

// VolumePrefix begins every managed volume name. It is what makes a volume
// recognisably ours during a prune, and what stops the agent from deleting a
// volume some other tool on the machine created.
const VolumePrefix = "latem-cache-"

// Label keys stamped on every managed volume.
//
// The name alone cannot be parsed back into its parts: "latem-cache-mac-mini-
// pnpm-store" splits as (mac, mini-pnpm-store) or (mac-mini, pnpm-store) with
// equal justification, because both a runner name and a cache name may contain
// a hyphen. RUNNER_SPEC §14.1 specifies the name and does not say how to read
// it back, so the labels carry the truth and the name stays human-legible.
const (
	LabelManaged = "latem.managed"
	LabelRunner  = "latem.runner"
	LabelCache   = "latem.cache"
)

// Defaults from RUNNER_SPEC §14.3, §14.4 and §13.
const (
	// DefaultLockTimeout is §14.3's ten minutes. After it, the run fails
	// E_CACHE rather than hanging forever on a contended cache.
	DefaultLockTimeout = 10 * time.Minute
	// DefaultMaxVolumeBytes is the per-cache ceiling, 25 GB.
	DefaultMaxVolumeBytes int64 = 25 << 30
	// DefaultWorkspaceMaxAge deletes workspaces with no live run after a day.
	DefaultWorkspaceMaxAge = 24 * time.Hour
	// StaleMirrorAge is the age at which a mirror is *reported* by
	// `latem doctor`. It is never auto-deleted: a mirror may be the only local
	// copy of a repository whose origin has since gone away.
	StaleMirrorAge = 90 * 24 * time.Hour
	// buildCacheKeep is §14.4's `buildx prune --filter until=168h`.
	buildCacheKeep = 168 * time.Hour
	// mirrorGCPrune is §14.4's `git gc --prune=30.days.ago`.
	mirrorGCPrune = "--prune=30.days.ago"
	// evictTarget is the fraction of the cap an eviction aims for. Evicting to
	// exactly the cap means the next build immediately trips it again.
	evictTarget = 0.8
	// lockFileName is §14.3's `<cache-root>/.latem-lock`.
	lockFileName = ".latem-lock"
)

// Per-runner disk floors from RUNNER_SPEC §13 and finding F12. Draft 01's
// single global 20 GB was sized for containers and would admit an iOS build
// onto a machine that cannot hold one — a Tart image carrying Xcode is ~90 GB
// before the build writes a byte.
const (
	DockerMinFreeGB float64 = 40
	NativeMinFreeGB float64 = 60
	TartMinFreeGB   float64 = 150
)

// DefaultMinFreeGB is the floor for a runner whose config does not set one.
// An unknown executor gets the strictest floor, because guessing low is the
// error that fails a build at 80%.
func DefaultMinFreeGB(executor string) float64 {
	switch executor {
	case ExecutorDocker:
		return DockerMinFreeGB
	case ExecutorNative:
		return NativeMinFreeGB
	case ExecutorTart:
		return TartMinFreeGB
	default:
		return TartMinFreeGB
	}
}

// namePattern shapes runner and cache names. It is the pattern the registry
// already enforces on caches[].name, repeated here because these names become
// Docker volume names and filesystem paths, and a package that builds a path
// from a string should be the one that checks it (see protocol.ValidateID).
//
// The leading class excludes "." and "-", so no name can be ".", "..", or a
// leading-dash string a command-line tool would read as a flag.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func validateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("cache: %s name is empty", kind)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("cache: %s name %q must match %s — it becomes a volume name and a path",
			kind, name, namePattern)
	}
	return nil
}

// VolumeName is RUNNER_SPEC §14.1's `latem-cache-<runner-name>-<cache-name>`.
//
// The runner is in the name, not the repo, and that is deliberate: package
// stores are content-addressed and benefit from sharing across repos, while
// per-runner scoping removes the cross-runner concurrency hazard without a
// lock in the hot path.
func VolumeName(runner, name string) string {
	return VolumePrefix + runner + "-" + name
}

// paths derives every path the cache subsystem uses from the agent root
// (~/.latem). Keeping them in one place means a layout change is one edit
// rather than a grep.
// paths locates the trees this package sweeps.
//
// work and mirrors are overridable because config.Paths makes them so:
// `agent.work_dir` relocates the work tree, and a relocated tree can sit on a
// different filesystem. Deriving them from root regardless meant FreeGB
// measured the wrong disk — silently, on exactly the gate (§10 steps 5 and 17)
// that exists to stop a build dying at 80% with a full disk (amendment A24).
type paths struct {
	root      string
	workDir   string
	mirrorDir string
}

func (p paths) caches() string { return filepath.Join(p.root, "caches") }

func (p paths) work() string {
	if p.workDir != "" {
		return p.workDir
	}
	return filepath.Join(p.root, "work")
}

func (p paths) mirrors() string {
	if p.mirrorDir != "" {
		return p.mirrorDir
	}
	return filepath.Join(p.root, "mirrors")
}

// cacheDir is §14.2's `~/.latem/caches/<runner-name>/<cache-name>/`.
func (p paths) cacheDir(runner, name string) string {
	return filepath.Join(p.caches(), runner, name)
}

// lockPath is the flock file for one cache.
//
// For a directory-backed cache this is §14.3 literally: `.latem-lock` inside
// the cache root. For a volume-backed cache there is no host-visible cache
// root to put it in — on Docker Desktop for macOS the volume lives inside the
// daemon's VM and /var/lib/docker is not a path this process can open — so the
// lock lives beside the cache tree instead. No cache name can collide with
// ".locks" because namePattern forbids a leading dot.
func (p paths) lockPath(runner, name string, volumeBacked bool) string {
	if volumeBacked {
		return filepath.Join(p.caches(), ".locks", runner, name+lockFileName)
	}
	return filepath.Join(p.cacheDir(runner, name), lockFileName)
}

// isDotName reports whether a directory entry is one of ours rather than a
// runner or cache. Names are validated against namePattern, which forbids a
// leading dot, so anything dotted is administrative.
func isDotName(name string) bool { return strings.HasPrefix(name, ".") }
