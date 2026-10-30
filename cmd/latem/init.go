package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

func newInitCommand() *cobra.Command {
	var (
		repos   []string
		refs    []string
		name    string
		force   bool
		printTo bool
	)
	cmd := &cobra.Command{
		Use:   "init --repo <name>=<url>",
		Short: "Write a starting config.toml for this machine",
		Long: `Write a starting config.toml for this machine.

Picks the executor from the platform — native on macOS, docker on Linux — and
gives the runner labels that match the reference jobs in examples/. Nothing
here needs a control plane: a machine that never registers is a supported
state, and ` + "`latem run`" + ` works against the local broker alone (§7.3).

Before this existed the only way to get a config was to copy the example out of
RUNNER_SPEC §8 by hand, or to run ` + "`latem register`" + `, which needs a control
plane URL and token that an open-source runner does not have. The agent's own
"no config file here" error pointed at exactly that dead end.

At least one repo is required and there is no placeholder default. A repo
absent from [repos.*] cannot be built (§8), so a config with a made-up entry is
a config that lies about what this machine will accept.

macOS gets the native executor because that is the default (§13.0): it needs
nothing installed beyond the Xcode you already have. It also has no isolation
of any kind. If this machine will build refs your team does not control, read
§13.0 and switch to tart before pointing anything at it.

Exit codes:
  0  config written
  1  a config already exists, or it could not be written
  2  usage`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(repos) == 0 {
				return usageErrorf("--repo is required, as --repo <name>=<url>\n" +
					"  e.g. --repo myapp=git@github.com:me/myapp.git\n" +
					"  the repo needs .latem/jobs.yaml at its ROOT — that is the repo the agent clones")
			}
			parsed, err := parseRepoFlags(repos)
			if err != nil {
				return usageErrorf("%v", err)
			}
			if len(refs) == 0 {
				refs = []string{"main"}
			}

			if name == "" {
				// register.go's version returns "" when the hostname would
				// not pass machineNamePattern — better an obvious placeholder
				// than a config the loader refuses.
				if name = defaultMachineName(); name == "" {
					name = "latem"
				}
			}
			body := renderInitConfig(initParams{
				Machine: name,
				GOOS:    runtime.GOOS,
				GOARCH:  runtime.GOARCH,
				Repos:   parsed,
				Refs:    refs,
			})

			if printTo {
				printf(cmd.OutOrStdout(), "%s", body)
				return nil
			}

			dir := configDir()
			path := filepath.Join(dir, config.FileName)
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; --force overwrites it", path)
			}
			// 0700 before writing, not after: the window between creating a
			// world-readable directory and tightening it is a window in which
			// secrets.env can be created inside it (§8).
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("creating %s: %w", dir, err)
			}
			// 0700 on a DIRECTORY, which gosec's G302 reads as a file mode.
			// §8 requires exactly this: the directory holds secrets.env and the
			// machine token, and a world-readable one is the threat model
			// failing quietly.
			if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // §8 requires 0700 on the config directory
				return fmt.Errorf("securing %s: %w", dir, err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}

			// Parse what was just written rather than trusting the template.
			// A generator that emits a config the loader rejects is worse than
			// no generator: it moves the failure to the agent, which reports it
			// as a broken installation rather than a broken template.
			if _, err := config.Parse([]byte(body), config.Options{Dir: dir}); err != nil {
				return fmt.Errorf("wrote %s but it does not load — this is a bug in `latem init`:\n%w", path, err)
			}

			printInitNext(cmd.OutOrStdout(), path, parsed)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "a repo to allow, as <name>=<url> (repeatable)")
	cmd.Flags().StringSliceVar(&refs, "allowed-refs", nil, "refs that may build without approval (default main)")
	cmd.Flags().StringVar(&name, "name", "", "machine name (default: this host's name)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config.toml")
	cmd.Flags().BoolVar(&printTo, "print", false, "write the config to stdout and exit")
	return cmd
}

type repoSpec struct{ Name, URL string }

type initParams struct {
	Machine, GOOS, GOARCH string
	Repos                 []repoSpec
	Refs                  []string
	// Docker adds a linux docker runner on macOS, beside the native one.
	// Linux always has one; it is the only executor there.
	Docker bool
	// Image is the docker runner's default_image; empty is latem-ubuntu.
	Image string
}

// parseRepoFlags splits --repo name=url. The name is what `latem run` takes and
// what [repos.*] is keyed by; it is not derived from the URL, because two
// remotes of the same project are a normal thing to have.
func parseRepoFlags(in []string) ([]repoSpec, error) {
	out := make([]repoSpec, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		name, url, ok := strings.Cut(s, "=")
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("--repo %q is not <name>=<url>", s)
		}
		if seen[name] {
			return nil, fmt.Errorf("--repo %s given twice", name)
		}
		seen[name] = true
		out = append(out, repoSpec{Name: name, URL: url})
	}
	return out, nil
}

func printInitNext(w io.Writer, path string, repos []repoSpec) {
	printf(w, "wrote %s\n\n", path)
	printf(w, "next:\n")
	printf(w, "  latem doctor            check this machine and name the fix for anything wrong\n")
	if len(repos) > 0 {
		// "clean" rather than "green": doctor warns about things that cost a
		// reboot rather than a build, and a machine carrying those can run a
		// job today. Telling an operator to wait for no warnings would stop
		// them on a machine that works.
		printf(w, "  latem run %s <job>   once doctor reports no failures\n", repos[0].Name)
	}
	printf(w, "  latem service install   start the agent at login and keep it running\n")
	if runtime.GOOS == "darwin" {
		printf(w, "\nnote: the macOS runner uses the native executor, which has NO isolation —\n")
		printf(w, "      a job runs as you, with your keychain. Read RUNNER_SPEC §13.0 before\n")
		printf(w, "      pointing this at refs your team does not control.\n")
	}
}
