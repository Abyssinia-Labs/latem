package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/source"
)

func newCacheCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and reclaim build caches",
		Long: `Inspect and reclaim build caches.

Caches here are mounted, not archived. Hosted CI needs save and restore
because its machines are ephemeral; yours are not. There is no tarball, no
upload, no size ceiling, and no cache step in the job (RUNNER_SPEC §14) —
a cache is a Docker named volume or a host directory that is still there when
the next run starts.

Caches are scoped per runner, never shared between runners, because two runs
writing one ~/.gradle corrupt each other in ways that surface as intermittent
build failures nobody attributes to caching (§14.3).`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newCacheLsCommand(), newCachePruneCommand(), newCacheRmCommand())
	return cmd
}

func newCacheLsCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List caches and their sizes",
		Long: `List caches and their sizes.

  NAME    RUNNER    SIZE    LAST USED    SOURCE

SIZE reads "unknown" for a volume whose driver cannot report one. That is not
zero: "unknown" and "empty" lead to opposite eviction decisions, and printing
0 B for a 20 GB volume would make a prune look like it had nothing to do.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, closeFn, err := openCacheManager()
			if err != nil {
				return err
			}
			defer closeFn()

			infos, err := mgr.List(cmd.Context())
			if err != nil {
				return err
			}
			warm, err := listWarmWorkspaces()
			if err != nil {
				return err
			}
			if len(infos) == 0 {
				printf(cmd.OutOrStdout(), "no caches yet — they appear the first time a job declares one\n")
				printWarmWorkspaces(cmd.OutOrStdout(), warm)
				return nil
			}
			sort.Slice(infos, func(i, j int) bool {
				if infos[i].Runner != infos[j].Runner {
					return infos[i].Runner < infos[j].Runner
				}
				return infos[i].Name < infos[j].Name
			})

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			printf(w, "NAME\tRUNNER\tSIZE\tLAST USED\tSOURCE\n")
			var total int64
			for _, in := range infos {
				if in.SizeBytes > 0 {
					total += in.SizeBytes
				}
				printf(w, "%s\t%s\t%s\t%s\t%s\n",
					in.Name, in.Runner, formatBytes(in.SizeBytes), formatAge(in.LastUsedAt), in.Source)
			}
			// Flushed before the summary, and the summary written to the
			// underlying writer rather than through the tabwriter. Sent
			// through it, a separator row of empty cells is padded to the
			// table's full width — a line of trailing spaces — and the
			// summary's own empty cells are padded to sit under columns it
			// has no values for.
			if err := w.Flush(); err != nil {
				return err
			}
			printf(cmd.OutOrStdout(), "\n%s, %s\n", countCaches(len(infos)), formatBytes(total))
			printWarmWorkspaces(cmd.OutOrStdout(), warm)
			return nil
		},
	}
}

// listWarmWorkspaces reads the workspaces jobs keep between runs (A90), from
// their own directory, through a manager that writes nothing.
func listWarmWorkspaces() ([]source.WarmInfo, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	reader, err := source.NewReader(source.Config{MirrorDir: cfg.Paths.Mirrors, WarmDir: cfg.Paths.Warm})
	if err != nil {
		// No git on this PATH is doctor's to report; the caches above are
		// still worth showing.
		return nil, nil
	}
	return reader.ListWarm()
}

// printWarmWorkspaces lists warm workspaces under the caches, when there are
// any: they are the other thing on this machine that makes a build fast and
// costs disk.
func printWarmWorkspaces(out io.Writer, warm []source.WarmInfo) {
	if len(warm) == 0 {
		return
	}
	printf(out, "\nwarm workspaces — kept by jobs that say `workspace: warm`\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	printf(w, "REPO\tJOB\tRUNNER\tSIZE\tLAST USED\tCOMMIT\n")
	var total int64
	for _, in := range warm {
		size := dirBytes(in.Path)
		total += size
		used := formatAge(in.UsedAt)
		if in.InUse {
			used = "in use"
		}
		printf(w, "%s\t%s\t%s\t%s\t%s\t%.12s\n", in.Repo, in.Job, in.Runner, formatBytes(size), used, in.SHA)
	}
	_ = w.Flush()
	printf(out, "\n%d warm, %s\n", len(warm), formatBytes(total))
}

// dirBytes is the apparent size of everything under path. A file that
// vanishes mid-walk, which a running build makes normal, is skipped.
func dirBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// countCaches renders the cache count with a noun that agrees with it. The
// summary used to read "1 caches".
func countCaches(n int) string {
	if n == 1 {
		return "1 cache"
	}
	return fmt.Sprintf("%d caches", n)
}

func newCachePruneCommand() *cobra.Command {
	var (
		aggressive bool
		maxSizeGB  float64
		maxAgeH    int
		warmAgeH   int
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Reclaim disk from caches, workspaces, mirrors, and leaked VMs",
		Long: `Reclaim disk per RUNNER_SPEC §14.4:

  - Docker build layers untouched for a week
  - cache volumes over their ceiling, evicted by access time down to 80%
  - workspaces under work/ older than a day with no live run
  - warm workspaces no run has used for agent.warm_retention (7 days unless
    set), or every one not in use with --aggressive; the next run of that job
    starts cold (A90)
  - git gc on each mirror, skipping any a live run is reading
  - Tart VMs named latem-run-* with no live run — a leaked one carrying an
    Xcode image costs about 90 GB, which is the fastest way to fill a Mac

Mirrors untouched for 90 days are reported by ` + "`latem doctor`" + ` and never
deleted here: a mirror may be the only local copy of a repository whose origin
has since been renamed, made private, or deleted.

Every sweep runs even if an earlier one failed. Abandoning the four sweeps that
would have freed 60 GB because tart is not installed leaves the machine exactly
as full as it was.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, closeFn, err := openCacheManager()
			if err != nil {
				return err
			}
			defer closeFn()

			opts := cache.PruneOptions{
				Aggressive:      aggressive,
				MaxVolumeBytes:  cache.DefaultMaxVolumeBytes,
				WorkspaceMaxAge: cache.DefaultWorkspaceMaxAge,
			}
			if maxSizeGB > 0 {
				opts.MaxVolumeBytes = int64(maxSizeGB * float64(1<<30))
			}
			if maxAgeH > 0 {
				opts.WorkspaceMaxAge = time.Duration(maxAgeH) * time.Hour
			}

			rep, pruneErr := mgr.Prune(cmd.Context(), opts)
			printPruneReport(cmd.OutOrStdout(), rep)
			warmErr := pruneWarmWorkspaces(cmd.OutOrStdout(), aggressive, warmAgeH)
			// The report is printed before the error is returned. A prune that
			// freed 40 GB and then failed its last sweep has still freed
			// 40 GB, and an operator needs to know which.
			return errors.Join(pruneErr, warmErr)
		},
	}
	cmd.Flags().BoolVar(&aggressive, "aggressive", false,
		"evict caches that would otherwise be kept — what the agent does below a runner's min_free_gb")
	cmd.Flags().Float64Var(&maxSizeGB, "max-size-gb", 0, "per-cache ceiling in GB (default 25)")
	cmd.Flags().IntVar(&maxAgeH, "workspace-max-age-hours", 0, "delete workspaces older than this (default 24)")
	cmd.Flags().IntVar(&warmAgeH, "warm-max-age-hours", 0,
		"delete warm workspaces unused for longer than this (default agent.warm_retention)")
	return cmd
}

// pruneWarmWorkspaces removes the warm workspaces nobody has used for long
// enough (A90), and prints which. One a run in the agent is building in is
// held by a lock this process cannot take, and is left.
func pruneWarmWorkspaces(out io.Writer, aggressive bool, maxAgeHours int) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(cfg.Paths.Warm); statErr != nil {
		return nil // no job here has ever kept one
	}
	mgr, err := source.New(source.Config{MirrorDir: cfg.Paths.Mirrors, WorkDir: cfg.Paths.Work, WarmDir: cfg.Paths.Warm})
	if err != nil {
		return err
	}
	retention := cfg.Agent.WarmRetention.Duration()
	if maxAgeHours > 0 {
		retention = time.Duration(maxAgeHours) * time.Hour
	}
	cutoff := time.Now().Add(-retention)
	if aggressive {
		cutoff = time.Time{}
	}
	gone, err := mgr.PruneWarm(cutoff)
	if len(gone) > 0 {
		printf(out, "\nwarm workspaces removed (%d)\n", len(gone))
		for _, w := range gone {
			printf(out, "  %s %s on %s, last used %s\n", w.Repo, w.Job, w.Runner, formatAge(w.UsedAt))
		}
	}
	return err
}

func printPruneReport(w io.Writer, rep cache.Report) {
	printf(w, "reclaimed %s\n", formatBytes(rep.BytesReclaimed))
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		printf(w, "\n%s (%d)\n", title, len(items))
		for _, it := range items {
			printf(w, "  %s\n", it)
		}
	}
	section("cache volumes evicted", rep.VolumesEvicted)
	section("workspaces removed", rep.WorkspacesGone)
	section("tart VMs reaped", rep.VMsReaped)
	section("mirrors collected", rep.MirrorsCollected)
}

func newCacheRmCommand() *cobra.Command {
	var runner string
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete one cache",
		Long: `Delete one cache.

Deleting a cache is safe for correctness and expensive for time: the next run
that declares it starts cold. Reach for it when a cache is corrupt — a
half-written Gradle metadata file, a node_modules from a different
architecture — and not to reclaim disk, which is ` + "`latem cache prune`" + `.

A cache name can exist on more than one runner, because the name embeds the
runner only in the volume (§14.1). Pass --runner when it does.

Run this while the agent is idle. This command does not take the per-cache
lock ` + "`latem agent`" + ` holds during a run, because the cache manager exposes no
way to take one without also provisioning the cache.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeCache(cmd.Context(), cmd.OutOrStdout(), args[0], runner)
		},
	}
	cmd.Flags().StringVar(&runner, "runner", "", "the runner whose copy of the cache to delete")
	return cmd
}

func removeCache(ctx context.Context, out io.Writer, name, runner string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	mgr, closeFn, err := openCacheManager()
	if err != nil {
		return err
	}
	defer closeFn()

	infos, err := mgr.List(ctx)
	if err != nil {
		return err
	}
	var matches []cache.Info
	for _, in := range infos {
		if in.Name == name && (runner == "" || in.Runner == runner) {
			matches = append(matches, in)
		}
	}
	switch {
	case len(matches) == 0:
		if runner != "" {
			return fmt.Errorf("no cache named %q on runner %q — `latem cache ls` lists what is there", name, runner)
		}
		return fmt.Errorf("no cache named %q — `latem cache ls` lists what is there", name)
	case len(matches) > 1:
		runners := make([]string, 0, len(matches))
		for _, m := range matches {
			runners = append(runners, m.Runner)
		}
		return usageErrorf("cache %q exists on %s; name one with --runner", name, strings.Join(runners, ", "))
	}

	in := matches[0]
	r, ok := cfg.Runner(in.Runner)
	if !ok {
		return fmt.Errorf("cache %q belongs to runner %q, which is no longer in config.toml", name, in.Runner)
	}

	if r.Executor == config.ExecutorDocker {
		api, closeAPI, err := openDockerCacheAPI()
		if err != nil {
			return err
		}
		defer closeAPI()
		volume := cache.VolumeName(in.Runner, in.Name)
		if err := api.VolumeRemove(ctx, volume); err != nil {
			return fmt.Errorf("removing volume %s: %w", volume, err)
		}
		printf(out, "removed volume %s (%s)\n", volume, formatBytes(in.SizeBytes))
		return nil
	}

	// Directory-backed caches for tart and native runners (§14.2). The path
	// comes from config's own derivation rather than being assembled here, so
	// relocating the config directory relocates this too.
	dir := filepath.Join(cfg.Paths.RunnerCacheDir(in.Runner), in.Name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing %s: %w", dir, err)
	}
	printf(out, "removed %s (%s)\n", dir, formatBytes(in.SizeBytes))
	return nil
}

// openCacheManager builds a cache manager from the agent's config.
//
// The Docker half is optional: a machine with only tart and native runners has
// no daemon and should still be able to list and prune its host-directory
// caches. A nil DockerAPI makes the volume-backed operations fail rather than
// silently substituting a directory no container will ever see.
func openCacheManager() (*cache.Local, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	runners := make(map[string]cache.RunnerConfig, len(cfg.Runners))
	needDocker := false
	for _, r := range cfg.Runners {
		runners[r.Name] = cache.RunnerConfig{Executor: string(r.Executor), MinFreeGB: r.MinFreeGB()}
		if r.Executor == config.ExecutorDocker {
			needDocker = true
		}
	}

	opts := cache.Options{Root: cfg.Paths.Dir, Runners: runners}
	closeFn := func() {}
	if needDocker {
		api, closeAPI, err := openDockerCacheAPI()
		if err == nil {
			opts.Docker, closeFn = api, closeAPI
		}
		// A daemon that is not reachable is doctor's problem to report, not a
		// reason `latem cache ls` should refuse to show the host directories
		// it can see.
	}
	mgr, err := cache.New(opts)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return mgr, closeFn, nil
}

func openDockerCacheAPI() (cache.DockerAPI, func(), error) {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the Docker daemon: %w", err)
	}
	return cache.NewDockerAPI(cli), func() { _ = cli.Close() }, nil
}

// formatBytes renders a size for a terminal.
//
// -1 is "unknown" rather than zero, because that is what a non-local volume
// driver reports and the two lead to opposite decisions.
func formatBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
