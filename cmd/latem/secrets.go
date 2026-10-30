package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

func newSecretsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Store, list and remove the secrets this machine's jobs read",
		Long: `Store, list and remove the secrets this machine's jobs read.

A workflow reads ${{ secrets.NAME }}; the machine answers it, never a
control plane (INV-2, §15). A name is looked up most specific first: for a
job in an environment, <repo>__<env>__NAME, then <repo>__NAME, then NAME —
so --repo and --env say which jobs a value is for, and a value with neither
is for every job on the machine.

Values go in the macOS Keychain (service "latem") on a Mac, and in
~/.latem/secrets.env at mode 0600 everywhere else; --store picks one. Either
is read afresh by every run, so nothing needs a reload. A value never travels
as an argument: ` + "`set`" + ` reads it from stdin, or from a prompt that does not
echo, and ` + "`list`" + ` prints names and never values.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newSecretsSetCommand(), newSecretsListCommand(), newSecretsRmCommand())
	return cmd
}

// scopeFlags are --repo and --env, shared by secrets and vars.
type scopeFlags struct {
	repo, env string
}

func (f *scopeFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.repo, "repo", "", "the repository it is for: its key, or owner/name (default: every repository)")
	cmd.Flags().StringVar(&f.env, "env", "", "the GitHub environment it is for (needs --repo)")
}

// secretScope is where a value applies, resolved against the config.
type secretScope struct {
	repo string // a [repos.*] key, or "" for the whole machine
	env  string
}

func (s secretScope) parts() []string {
	switch {
	case s.repo == "":
		return nil
	case s.env == "":
		return []string{s.repo}
	default:
		return []string{s.repo, s.env}
	}
}

func (s secretScope) String() string {
	switch {
	case s.repo == "":
		return "every repository"
	case s.env == "":
		return "repo " + s.repo
	default:
		return "repo " + s.repo + ", environment " + s.env
	}
}

// resolve turns --repo and --env into a scope. --repo must name a repository
// this machine builds: a secret scoped to a key nothing uses is a value no run
// will ever read.
func (f scopeFlags) resolve() (secretScope, error) {
	if f.env != "" && f.repo == "" {
		return secretScope{}, usageErrorf("--env needs --repo: an environment belongs to a repository")
	}
	if f.repo == "" {
		return secretScope{}, nil
	}
	key, err := resolveRepoKey(f.repo)
	if err != nil {
		return secretScope{}, err
	}
	return secretScope{repo: key, env: strings.TrimSpace(f.env)}, nil
}

// storedName is the name a value is filed under: name with the scope's
// parts, exactly as the run's lookup builds it (secrets.ScopedName).
func storedName(name string, scope secretScope) (string, error) {
	if err := secrets.ValidName(name); err != nil {
		return "", usageErrorf("%v", err)
	}
	stored := secrets.ScopedName(name, scope.parts()...)
	if err := secrets.ValidName(stored); err != nil {
		// A repository key that starts with a digit makes a name no
		// provider can hold, and the run's lookup skips that scope.
		return "", fmt.Errorf("%s cannot scope a secret: %q is not a name a secret store can hold (%v); "+
			"re-add the repository under a --key that starts with a letter", scope, stored, err)
	}
	return stored, nil
}

// secretStores are the places a value can go on this machine. keychain is nil
// off macOS.
type secretStores struct {
	keychain secrets.Store
	file     secrets.Store
}

// newSecretStores is a seam: a test must never write to, list, or delete from
// the Keychain of the machine running it.
var newSecretStores = func(paths config.Paths) (secretStores, error) {
	st := secretStores{file: secrets.NewFileStore(paths.SecretsEnv)}
	if runtime.GOOS == "darwin" {
		kc, err := secrets.NewKeychainStore("", secrets.ExecRunner{})
		if err != nil {
			return st, err
		}
		st.keychain = kc
	}
	return st, nil
}

// pick chooses the store --store names, defaulting to the platform's.
func (s secretStores) pick(choice string) (secrets.Store, error) {
	switch choice {
	case "", "auto":
		if s.keychain != nil {
			return s.keychain, nil
		}
		return s.file, nil
	case "keychain":
		if s.keychain == nil {
			return nil, usageErrorf("--store keychain: there is no Keychain on %s; use --store file", runtime.GOOS)
		}
		return s.keychain, nil
	case "file":
		return s.file, nil
	}
	return nil, usageErrorf("--store %q: one of keychain, file", choice)
}

// all lists the stores that exist here, in the order the chain reads them —
// secrets.env before the Keychain (§15).
func (s secretStores) all() []secrets.Store {
	out := []secrets.Store{s.file}
	if s.keychain != nil {
		out = append(out, s.keychain)
	}
	return out
}

func newSecretsSetCommand() *cobra.Command {
	var (
		scope scopeFlags
		store string
	)
	cmd := &cobra.Command{
		Use:   "set [--repo R] [--env E] NAME",
		Short: "Store a secret's value, read from stdin or a hidden prompt",
		Long: `Store a secret's value, read from stdin or a hidden prompt.

  latem secrets set --repo latem-cloud --env production CONVEX_DEPLOY_KEY
  pbpaste | latem secrets set CLOUDFLARE_API_TOKEN
  latem secrets set --repo app APP_STORE_CONNECT_KEY_P8 < AuthKey_ABC123.p8

Piped in, a one-line value loses its trailing newline — what echo and
pbpaste add — and a multi-line one, a PEM, is kept byte for byte. There is no
way to pass the value as an argument, because an argument is in your shell
history and in every user's process listing.

Setting a name that already has a value replaces it.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageErrorf("%s takes the NAME only; the value is read from stdin or the prompt, "+
					"never from the command line, where it would land in shell history", cmd.CommandPath())
			}
			return exactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := scope.resolve()
			if err != nil {
				return err
			}
			name := args[0]
			stored, err := storedName(name, sc)
			if err != nil {
				return err
			}
			paths := config.DerivePaths(configDir())
			stores, err := newSecretStores(paths)
			if err != nil {
				return err
			}
			dst, err := stores.pick(store)
			if err != nil {
				return err
			}

			value, err := readSecretValue(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), name, sc)
			if err != nil {
				return err
			}
			defer value.Zero()
			if err := dst.Set(cmd.Context(), stored, value); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			printf(out, "stored %s for %s, in the %s as %s\n", name, sc, storeWord(dst), stored)
			if logstream.TooShort(value) {
				printf(out, "\nwarning: values shorter than %d bytes are not redacted from build logs (§16)\n",
					logstream.MinRedactableLength)
			}
			// secrets.env is read before the Keychain, so a stale entry there
			// would answer instead of the value just stored.
			if dst == stores.keychain {
				if names, err := stores.file.Names(cmd.Context()); err == nil && slices.Contains(names, stored) {
					printf(out, "\nwarning: %s also defines %s, and is read first — remove it so this value is used:\n"+
						"  latem secrets rm --store file %s\n", paths.SecretsEnv, stored, scopeArgs(sc, name))
				}
			}
			return nil
		},
	}
	scope.register(cmd)
	cmd.Flags().StringVar(&store, "store", "auto", "where to keep it: keychain (macOS default) or file (secrets.env)")
	return cmd
}

// storeWord names a store in a sentence.
func storeWord(s secrets.Store) string {
	if s.Name() == "keychain" {
		return "Keychain"
	}
	return "secrets.env"
}

// scopeArgs is the --repo/--env/NAME tail of a command line for a scope.
func scopeArgs(sc secretScope, name string) string {
	var b strings.Builder
	if sc.repo != "" {
		b.WriteString("--repo " + sc.repo + " ")
	}
	if sc.env != "" {
		b.WriteString("--env " + sc.env + " ")
	}
	return b.String() + name
}

// readSecretValue reads the value from a terminal without echoing it, or
// from whatever is piped in.
func readSecretValue(ctx context.Context, in io.Reader, prompt io.Writer, name string, sc secretScope) (secrets.Value, error) {
	var value []byte
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		line, err := readHidden(ctx, f, prompt, fmt.Sprintf("value for %s (%s, not echoed)", name, sc))
		if err != nil {
			return nil, err
		}
		value = line
	} else {
		data, err := io.ReadAll(io.LimitReader(in, secrets.MaxFileSize+1))
		if err != nil {
			return nil, fmt.Errorf("reading the value from stdin: %w", err)
		}
		if len(data) > secrets.MaxFileSize {
			secrets.Value(data).Zero()
			return nil, fmt.Errorf("the value on stdin is larger than %d bytes", secrets.MaxFileSize)
		}
		value = trimPipedValue(data)
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("no value arrived for %s; an empty secret is GitHub's unset one — "+
			"`latem secrets rm` removes a value instead", name)
	}
	return secrets.Value(value), nil
}

// trimPipedValue drops the newline `echo` and `pbpaste` end a one-line value
// with. A value with a newline anywhere else is multi-line — a PEM — and its
// final newline is part of it.
func trimPipedValue(b []byte) []byte {
	if bytes.Count(b, []byte("\n")) != 1 || !bytes.HasSuffix(b, []byte("\n")) {
		return b
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

// isTerminal reports whether f is a character device — a terminal, for the
// purposes of deciding to prompt. A stat, not a terminal library, for the
// reason useColor gives.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// readHidden prompts on prompt and reads one line from the terminal f with
// echo off, through stty(1) rather than a terminal library. The terminal's
// settings are restored on every path out, a cancelled context included.
//
// Canonical mode goes off with echo. The terminal's line discipline holds at
// most MAX_CANON bytes of a line — 1024 on macOS — and rings the bell at the
// rest, newline included, so a long token pasted at the prompt never arrived
// and the command waited for ever. Read a byte at a time instead, this does
// the editing a prompt needs: erase, kill the line, end of input.
func readHidden(ctx context.Context, f *os.File, prompt io.Writer, label string) ([]byte, error) {
	printf(prompt, "%s: ", label)
	saved, err := sttySettings(f)
	if err != nil {
		return nil, fmt.Errorf("reading the terminal's settings: %w; pipe the value in instead", err)
	}
	if err := stty(f, "-echo", "-icanon", "min", "1", "time", "0"); err != nil {
		_ = stty(f, saved)
		return nil, fmt.Errorf("turning off echo: %w; pipe the value in instead", err)
	}
	defer func() {
		_ = stty(f, saved)
		printf(prompt, "\n")
	}()

	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readTypedLine(bufio.NewReader(f))
		done <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil && !errors.Is(r.err, io.EOF) {
			return nil, r.err
		}
		return r.line, nil
	}
}

// readTypedLine reads one line from a terminal in non-canonical mode, applying
// the edits canonical mode would have: erase removes the last character
// rather than the last byte, ^U clears the line, ^D ends input.
func readTypedLine(r io.ByteReader) ([]byte, error) {
	var line []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return line, err
		}
		switch b {
		case '\n', '\r':
			return line, nil
		case 0x7f, 0x08: // DEL, BS
			if len(line) > 0 {
				_, n := utf8.DecodeLastRune(line)
				line = line[:len(line)-n]
			}
		case 0x15: // ^U
			line = line[:0]
		case 0x04: // ^D
			if len(line) == 0 {
				return line, io.EOF
			}
			return line, nil
		default:
			line = append(line, b)
		}
	}
}

// sttySettings is the terminal's state as `stty -g` prints it, which stty
// takes back as one argument.
func sttySettings(f *os.File) (string, error) {
	c := exec.Command("stty", "-g")
	c.Stdin = f
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}

func stty(f *os.File, args ...string) error {
	// #nosec G204 -- args are the literals above, or what `stty -g` printed;
	// there is no shell.
	c := exec.Command("stty", args...)
	c.Stdin = f
	return c.Run()
}

func newSecretsListCommand() *cobra.Command {
	var scope scopeFlags
	cmd := &cobra.Command{
		Use:   "list [--repo R] [--env E]",
		Short: "List the secrets stored on this machine, by name and scope",
		Long: `List the secrets stored on this machine, by name and scope.

Names only, never values: nothing is decrypted to list them. The Keychain's
items are read from their attributes, secrets.env's from the names its lines
declare, and [secrets.commands] from config.toml. LATEM_SECRET_* variables in
the agent's own environment answer too and are not listed: that environment
is the service manager's, not this shell's.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc, err := scope.resolve()
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			stores, err := newSecretStores(cfg.Paths)
			if err != nil {
				return err
			}
			inv, err := secretInventory(cmd.Context(), cfg, stores)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, e := range inv.entries(cfg) {
				if sc.repo != "" && (e.scope.repo != sc.repo || (sc.env != "" && e.scope.env != sc.env)) {
					continue
				}
				where := strings.Join(e.stores, ", ")
				if len(e.stores) > 1 {
					where += " (" + e.stores[0] + " answers)"
				}
				rows = append(rows, []string{e.scopeLabel(), e.name, where})
			}
			if len(rows) == 0 {
				printf(cmd.ErrOrStderr(), "no secrets stored for %s — `latem secrets set` stores one\n", sc)
				return nil
			}
			printTable(cmd.OutOrStdout(), []string{"SCOPE", "NAME", "STORE"}, rows)
			return nil
		},
	}
	scope.register(cmd)
	return cmd
}

func newSecretsRmCommand() *cobra.Command {
	var (
		scope scopeFlags
		store string
	)
	cmd := &cobra.Command{
		Use:   "rm [--repo R] [--env E] NAME",
		Short: "Remove a secret's value",
		Long: `Remove a secret's value.

Removes it from every store that holds it under this scope, or from the one
--store names. A wider scope's value — the machine-wide NAME, when this was
--repo's — is not touched, and is what a run reads from now on if it exists.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := scope.resolve()
			if err != nil {
				return err
			}
			stored, err := storedName(args[0], sc)
			if err != nil {
				return err
			}
			stores, err := newSecretStores(config.DerivePaths(configDir()))
			if err != nil {
				return err
			}
			targets := stores.all()
			if store != "" && store != "auto" {
				one, err := stores.pick(store)
				if err != nil {
					return err
				}
				targets = []secrets.Store{one}
			}
			out := cmd.OutOrStdout()
			removed := 0
			for _, s := range targets {
				ok, err := s.Remove(cmd.Context(), stored)
				if err != nil {
					return err
				}
				if ok {
					removed++
					printf(out, "removed %s (%s) from the %s\n", args[0], sc, storeWord(s))
				}
			}
			if removed == 0 {
				return fmt.Errorf("no value for %s is stored for %s", args[0], sc)
			}
			return nil
		},
	}
	scope.register(cmd)
	cmd.Flags().StringVar(&store, "store", "auto", "remove from this store only: keychain or file")
	return cmd
}

// inventory is every stored secret name on the machine and where each is
// held. It is built from names alone; no value is read.
type inventory struct {
	// byName maps a stored (scoped) name to its stores, in chain order.
	byName map[string][]string
}

// secretInventory lists stored names from every store and from
// [secrets.commands].
func secretInventory(ctx context.Context, cfg *config.Config, stores secretStores) (inventory, error) {
	inv := inventory{byName: map[string][]string{}}
	for _, s := range stores.all() {
		if s == nil {
			continue // none configured: doctor reads needs, not secrets
		}
		names, err := s.Names(ctx)
		if err != nil {
			return inv, err
		}
		for _, n := range names {
			inv.byName[n] = append(inv.byName[n], s.Name())
		}
	}
	for n := range cfg.Secrets.Commands {
		inv.byName[n] = append(inv.byName[n], "command")
	}
	return inv, nil
}

// has reports whether name resolves anywhere for a job of repo in env, the
// way the run looks it up (internal/run's resolveWorkflowSecrets): repo and
// environment, then repo, then the machine.
func (inv inventory) has(name, repo, env string) bool {
	scopes := [][]string{{repo}, nil}
	if repo == "" {
		// A checkout this machine has no [repos.*] entry for: only the
		// machine-wide name could answer.
		scopes = [][]string{nil}
	} else if env != "" {
		scopes = append([][]string{{repo, env}}, scopes...)
	}
	for _, sc := range scopes {
		if len(inv.byName[secrets.ScopedName(name, sc...)]) > 0 {
			return true
		}
	}
	return false
}

// inventoryEntry is one stored name, decoded into its scope.
type inventoryEntry struct {
	scope  secretScope
	name   string
	stores []string
}

func (e inventoryEntry) scopeLabel() string {
	switch {
	case e.scope.repo == "":
		return "*"
	case e.scope.env == "":
		return e.scope.repo
	default:
		return e.scope.repo + " / " + e.scope.env
	}
}

// entries decodes every stored name back into a scope and a name.
//
// The scoped form is lossy — ScopedName reduces a key to [A-Za-z0-9_] — so a
// prefix counts as a repository only when it is the scoped form of a key this
// config has, and a machine-wide name that happens to contain __ is left
// alone. Under a repository, a third part is the environment: the one reading
// the run's lookup cannot tell from a repo-scoped name that itself holds __,
// and that name would be one nobody chose on purpose.
func (inv inventory) entries(cfg *config.Config) []inventoryEntry {
	repoOf := map[string]string{}
	for key := range cfg.Repos {
		repoOf[strings.TrimSuffix(secrets.ScopedName("", key), secrets.ScopeSeparator)] = key
	}
	var out []inventoryEntry
	for _, stored := range slices.Sorted(maps.Keys(inv.byName)) {
		e := inventoryEntry{name: stored, stores: inv.byName[stored]}
		parts := strings.SplitN(stored, secrets.ScopeSeparator, 3)
		if key, ok := repoOf[parts[0]]; ok && len(parts) >= 2 {
			e.scope.repo = key
			e.name = strings.Join(parts[1:], secrets.ScopeSeparator)
			if len(parts) == 3 {
				e.scope.env, e.name = parts[1], parts[2]
			}
		}
		out = append(out, e)
	}
	return out
}
