package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// newSecretReader reads a stored value back, from the two places `secrets
// set` puts one, in the order a run reads them. Not [secrets.New]'s chain: a
// LATEM_SECRET_* in this shell or a [secrets.commands] entry is not a value
// stored on this machine, and copying one would make it a different thing on
// the other. A seam, for the reason newSecretStores is one.
var newSecretReader = func(paths config.Paths) (secrets.Provider, error) {
	providers := []secrets.Provider{secrets.NewFileProvider(paths.SecretsEnv)}
	if runtime.GOOS == "darwin" {
		kc, err := secrets.NewKeychainProvider("", secrets.ExecRunner{})
		if err != nil {
			return nil, err
		}
		providers = append(providers, kc)
	}
	return secrets.NewChain(providers...), nil
}

// runSSH runs ssh with stdin. A seam: a test has no other machine.
var runSSH = func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	// #nosec G204 -- argv[0] is "ssh"; the host is the operator's own, after
	// "--", and the remote command is built from validated names, quoted.
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	return c.Run()
}

func newSecretsCopyCommand() *cobra.Command {
	var (
		scope    scopeFlags
		to       string
		identity string
		store    string
	)
	cmd := &cobra.Command{
		Use:   "copy --to HOST [--repo R] [--env E] NAME...",
		Short: "Copy secrets to another machine over ssh",
		Long: `Copy secrets to another machine over ssh.

  latem secrets copy --to mini --repo tsedal --env staging CLOUDFLARE_API_TOKEN
  latem secrets copy --to me@desktop.local -i ~/.ssh/latem MATCH_PASSWORD

Each value is read from where ` + "`secrets set`" + ` put it here, under the same
--repo and --env, and handed to ` + "`latem secrets set`" + ` on HOST on its stdin.
It is never an argument on either machine, never on this terminal, and
never in a file on the way. The other machine files it under the same
scope; --repo goes as owner/name, so its key for the repository may differ.

HOST is anything ssh takes, an alias from ~/.ssh/config included. The other
machine's latem is found on its PATH, or in ~/.local/bin, where install.sh
puts it. The first name that does not copy stops the rest.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("%s needs the NAME of at least one secret", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return usageErrorf("--to is required: the machine to copy to, as ssh names it")
			}
			if store != "" && store != "auto" && store != "keychain" && store != "file" {
				return usageErrorf("--store %q: one of keychain, file", store)
			}
			sc, err := scope.resolve()
			if err != nil {
				return err
			}
			stored := make([]string, len(args))
			for i, name := range args {
				if stored[i], err = storedName(name, sc); err != nil {
					return err
				}
			}
			remoteRepo := ""
			if sc.repo != "" {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				remoteRepo = sc.repo
				if slug, ok := cfg.Repos[sc.repo].GitHubRepository(); ok {
					remoteRepo = slug
				}
			}
			reader, err := newSecretReader(config.DerivePaths(configDir()))
			if err != nil {
				return err
			}

			ctx, out := cmd.Context(), cmd.OutOrStdout()
			for i, name := range args {
				value, err := reader.Resolve(ctx, stored[i])
				if errors.Is(err, secrets.ErrNotFound) {
					return fmt.Errorf("no value for %s is stored for %s on this machine, so it and the names after it "+
						"were not copied; `latem secrets list` shows what is", name, sc)
				}
				if err != nil {
					return fmt.Errorf("reading %s: %w", name, err)
				}
				argv := sshArgv(to, identity, remoteSetCommand(remoteRepo, sc.env, store, name))
				err = runSSH(ctx, argv, bytes.NewReader(value), out, cmd.ErrOrStderr())
				value.Zero()
				if err != nil {
					return fmt.Errorf("%s did not store %s (%w); its own words are above, and nothing after it was sent",
						to, name, err)
				}
			}
			return nil
		},
	}
	scope.register(cmd)
	cmd.Flags().StringVar(&to, "to", "", "the machine to copy to: a host, user@host, or an alias from ~/.ssh/config")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "the ssh key to use, and no other")
	cmd.Flags().StringVar(&store, "store", "auto", "where the other machine keeps it: keychain (macOS default) or file")
	return cmd
}

// sshArgv is ssh to host running command. Host goes after --, so a host that
// begins with a dash is a host and not an option.
func sshArgv(host, identity, command string) []string {
	argv := []string{"ssh"}
	if identity != "" {
		argv = append(argv, "-i", identity, "-o", "IdentitiesOnly=yes")
	}
	return append(argv, "--", host, command)
}

// remoteSetCommand is the command line `secrets set` runs as on the other
// machine. ssh hands it to that machine's shell, which is not a login shell
// and on a Mac has no ~/.local/bin on its PATH.
func remoteSetCommand(repo, env, store, name string) string {
	words := []string{"latem", "secrets", "set"}
	if repo != "" {
		words = append(words, "--repo", repo)
	}
	if env != "" {
		words = append(words, "--env", env)
	}
	if store != "" && store != "auto" {
		words = append(words, "--store", store)
	}
	words = append(words, "--", name)
	for i, w := range words {
		words[i] = shellWord(w)
	}
	return `PATH="$HOME/.local/bin:$PATH" ` + strings.Join(words, " ")
}

// shellWord quotes w for a POSIX shell when it needs it.
func shellWord(w string) string {
	if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./") == "" {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}
