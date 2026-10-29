package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

func newRepoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Add, list and remove the repositories this machine builds",
		Long: `Add, list and remove the repositories this machine builds.

[repos.*] in config.toml is the allowlist (§8, INV-2): a repository absent
from it cannot be built, whatever a control plane asks for. These commands
edit that table and nothing else, keeping the rest of the file — your
comments included — byte for byte, then ask the running agent to reload.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newRepoAddCommand(), newRepoListCommand(), newRepoRemoveCommand())
	return cmd
}

func newRepoAddCommand() *cobra.Command {
	var (
		key      string
		refs     []string
		approval bool
		noVerify bool
	)
	cmd := &cobra.Command{
		Use:   "add <owner/name>",
		Short: "Allow this machine to build a GitHub repository",
		Long: `Allow this machine to build a GitHub repository.

Writes [repos.<name>] with the repository's https URL and the refs it may
build without approval: its default branch, and refs/pull/*/merge, which is
what a pull request's checks run on. --allowed-refs replaces both.

The repository is checked first with git ls-remote, run the way the agent
runs git — with no terminal to prompt on and without your shell's ssh agent,
which a service does not inherit. A repository you can read and the service
cannot is refused here, rather than failing every run at fetch; --no-verify
writes it anyway.

The table is keyed by the repository's name, lowercased: that key is what
` + "`latem run`" + `, ` + "`--repo`" + ` on secrets and vars, and the control plane call it.
--key picks another, for two repositories of the same name.

Exit codes:
  0  added (and the running agent reloaded, or told why not)
  1  the repository could not be read, is already allowed, or the file could
     not be written
  2  usage`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, err := parseGitHubSlug(args[0])
			if err != nil {
				return usageErrorf("%v", err)
			}
			if key == "" {
				key = repoKeyFor(slug)
			}
			return runRepoAdd(cmd, repoAddFlags{slug: slug, key: key, refs: refs, approval: approval, noVerify: noVerify})
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "the name to file it under (default: the repository's name)")
	cmd.Flags().StringSliceVar(&refs, "allowed-refs", nil,
		"refs that may build without approval (default: the default branch and refs/pull/*/merge)")
	cmd.Flags().BoolVar(&approval, "require-approval", false,
		"park any other ref for `latem approve` instead of rejecting it")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "write the entry without checking that the repository is readable")
	return cmd
}

type repoAddFlags struct {
	slug, key string
	refs      []string
	approval  bool
	noVerify  bool
}

func runRepoAdd(cmd *cobra.Command, f repoAddFlags) error {
	out := cmd.OutOrStdout()
	paths := config.DerivePaths(configDir())
	url := "https://github.com/" + f.slug + ".git"

	if cfg, err := loadConfig(); err == nil {
		if r, ok := cfg.Repos[f.key]; ok {
			return fmt.Errorf("repos.%s is already in %s (%s); remove it first with `latem repo remove %s`, "+
				"or pick another name with --key", f.key, paths.File, r.URL, f.key)
		}
		for _, k := range slices.Sorted(maps.Keys(cfg.Repos)) {
			if slug, ok := cfg.Repos[k].GitHubRepository(); ok && strings.EqualFold(slug, f.slug) {
				return fmt.Errorf("%s is already allowed, as repos.%s", f.slug, k)
			}
		}
	}

	refs := f.refs
	if !f.noVerify {
		printf(out, "checking %s the way the agent will fetch it …\n", url)
		head, err := verifyRepo(cmd.Context(), url)
		if err != nil {
			return err
		}
		if len(refs) == 0 && head != "" {
			refs = []string{head, pullRequestRefs}
		}
	}
	if len(refs) == 0 {
		refs = []string{"main", pullRequestRefs}
	}

	table := tomlHeader("repos", f.key) + "\n" +
		"url          = " + tomlString(url) + "\n" +
		"allowed_refs = " + tomlStringList(refs) + "\n"
	if f.approval {
		table += "require_approval_otherwise = true\n"
	}
	warning, err := editConfig(paths, func(body string) (string, error) {
		return insertAfter(body, []string{"repos"}, table), nil
	})
	if err != nil {
		return err
	}
	printf(out, "added repos.%s to %s\n  url           %s\n  allowed refs  %s\n",
		f.key, paths.File, url, strings.Join(refs, ", "))
	if warning != "" {
		printf(out, "\n%s\n", warning)
	}
	printf(out, "\n")
	announceConfigChange(out, paths)
	printf(out, "\nnext:\n  latem workflows check <a checkout of %s>   what this machine can run, and what it needs\n"+
		"  latem secrets set --repo %s <NAME>          a secret its workflows read\n", f.slug, f.key)
	return nil
}

// pullRequestRefs is what a pull request's checks are built from: GitHub's
// merge of the branch into its base (PROTOCOL.md §3.2).
const pullRequestRefs = "refs/pull/*/merge"

// parseGitHubSlug accepts owner/name, or a github.com URL in any form git
// takes, and returns owner/name.
func parseGitHubSlug(arg string) (string, error) {
	s := strings.TrimSpace(arg)
	url := s
	if !strings.Contains(s, "://") && !strings.HasPrefix(s, "git@") {
		url = "https://github.com/" + strings.TrimSuffix(s, ".git") + ".git"
	}
	slug, ok := config.Repo{URL: url}.GitHubRepository()
	if !ok {
		return "", fmt.Errorf("%q is not a GitHub repository; give it as owner/name", arg)
	}
	return slug, nil
}

// repoKeyFor is the config key a repository is filed under by default: its
// name, lowercased, in the characters a key may hold (§8 — it becomes the
// mirror's directory name).
func repoKeyFor(slug string) string {
	_, name, _ := strings.Cut(slug, "/")
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	key := strings.TrimLeft(b.String(), "._-")
	if len(key) > 64 {
		key = key[:64]
	}
	return key
}

// gitStateVars redirect git at a repository other than the one it was asked
// about. They are internal/source's list (gitStateEnv), restated because that
// package exports no way to build the agent's git environment; a probe that
// inherited GIT_DIR from the shell it was typed in would answer for the wrong
// repository.
var gitStateVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
}

// serviceGitEnv is the environment the agent's git runs with, as nearly as a
// command typed in a terminal can reproduce it: the git state variables
// dropped and prompting off, as internal/source does, and — unless keepAgent —
// no SSH_AUTH_SOCK, because launchd and systemd do not hand the agent your
// shell's ssh agent. The doctor's repo check makes the same subtraction.
func serviceGitEnv(keepAgent bool) []string {
	drop := map[string]bool{"GIT_TERMINAL_PROMPT": true}
	for _, v := range gitStateVars {
		drop[v] = true
	}
	if !keepAgent {
		drop["SSH_AUTH_SOCK"] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && drop[name] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// lsRemote runs `git ls-remote --symref <url> HEAD`. A seam: no test reaches
// a real remote.
var lsRemote = func(ctx context.Context, url string, keepAgent bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// --end-of-options, per amendment A7: the URL is an argument, never an
	// option, whatever it starts with.
	//
	// #nosec G204 -- argv, no shell; url is https://github.com/<owner>/<name>.git
	// built from a slug the GitHub pattern already constrained, and it follows
	// --end-of-options.
	c := exec.CommandContext(ctx, "git", "ls-remote", "--symref", "--end-of-options", url, "HEAD")
	c.Env = serviceGitEnv(keepAgent)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

// verifyRepo proves the agent will be able to fetch url, and returns the
// repository's default branch when it says.
func verifyRepo(ctx context.Context, url string) (string, error) {
	out, err := lsRemote(ctx, url, false)
	if err == nil {
		return defaultBranch(out), nil
	}
	if _, withAgent := lsRemote(ctx, url, true); withAgent == nil {
		return "", fmt.Errorf("%s is readable from your shell and not from a service: it answered with your "+
			"SSH_AUTH_SOCK and refused without it, and the agent runs without it. Nothing was written.\n"+
			"Give the machine a credential a service can use — see `latem doctor` — or pass --no-verify", url)
	}
	detail := gitRemoteError(out)
	if detail == "" {
		detail = err.Error()
	}
	notes, fixes := repoFailureAdvice(url, out)
	var b strings.Builder
	fmt.Fprintf(&b, "%s could not be read the way the agent will read it: %s\nnothing was written.", url, detail)
	for _, n := range notes {
		fmt.Fprintf(&b, "\n\n%s", n)
	}
	if len(fixes) > 0 {
		b.WriteString("\n\nfix:")
		for _, f := range fixes {
			fmt.Fprintf(&b, "\n  %s", f)
		}
	}
	b.WriteString("\n\nor write it anyway with --no-verify")
	return "", errors.New(b.String())
}

// defaultBranch reads `ref: refs/heads/<branch>	HEAD` from ls-remote --symref.
func defaultBranch(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if branch, _, ok := strings.Cut(rest, "\t"); ok && branch != "" {
				return branch
			}
		}
	}
	return ""
}

func newRepoListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the repositories this machine builds",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(cfg.Repos) == 0 {
				printf(cmd.ErrOrStderr(), "no repositories yet — `latem repo add <owner/name>` allows one\n")
				return nil
			}
			rows := make([][]string, 0, len(cfg.Repos))
			for _, k := range slices.Sorted(maps.Keys(cfg.Repos)) {
				r := cfg.Repos[k]
				slug, ok := r.GitHubRepository()
				if !ok {
					slug = "-"
				}
				refs := strings.Join(r.AllowedRefs, ",")
				if r.RequireApprovalOtherwise {
					refs += " (+approval)"
				}
				rows = append(rows, []string{k, slug, refs, r.URL})
			}
			printTable(out, []string{"KEY", "REPOSITORY", "ALLOWED REFS", "URL"}, rows)
			return nil
		},
	}
}

func newRepoRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <key|owner/name>",
		Short: "Stop this machine building a repository",
		Long: `Stop this machine building a repository.

Removes [repos.<key>] and its vars and environments from config.toml, and asks
the running agent to reload. Secrets stored for the repository are not
touched — ` + "`latem secrets list --repo <key>`" + ` shows them — and neither is its
mirror, which the daily maintenance pass ages out.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			paths := config.DerivePaths(configDir())
			key, err := resolveRepoKey(args[0])
			if err != nil {
				return err
			}
			removed := 0
			warning, err := editConfig(paths, func(body string) (string, error) {
				var updated string
				updated, removed = removeTables(body, []string{"repos", key})
				return updated, nil
			})
			if err != nil {
				return err
			}
			if removed == 0 {
				return fmt.Errorf("there is no [repos.%s] table in %s to remove; it may be written inline — edit the file by hand",
					key, paths.File)
			}
			printf(out, "removed repos.%s from %s\n", key, paths.File)
			if warning != "" {
				printf(out, "\n%s\n", warning)
			}
			printf(out, "\n")
			announceConfigChange(out, paths)
			return nil
		},
	}
}

// resolveRepoKey finds the config key an argument names: a key as written, or
// the owner/name of a repository the config allows.
func resolveRepoKey(arg string) (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if _, ok := cfg.Repos[arg]; ok {
		return arg, nil
	}
	if strings.Contains(arg, "/") {
		for _, k := range slices.Sorted(maps.Keys(cfg.Repos)) {
			if slug, ok := cfg.Repos[k].GitHubRepository(); ok && strings.EqualFold(slug, arg) {
				return k, nil
			}
		}
	}
	keys := slices.Sorted(maps.Keys(cfg.Repos))
	if len(keys) == 0 {
		return "", fmt.Errorf("no repo %q: this machine has no repositories yet", arg)
	}
	return "", fmt.Errorf("no repo %q on this machine; it has %s", arg, strings.Join(keys, ", "))
}

// printTable prints rows under a header: aligned for a terminal, raw tabs for
// a pipe, for the reason listRuns gives.
func printTable(w io.Writer, header []string, rows [][]string) {
	st := newStyler(w)
	if !st.color {
		printf(w, "%s\n", strings.Join(header, "\t"))
		for _, row := range rows {
			printf(w, "%s\n", strings.Join(row, "\t"))
		}
		return
	}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		for i, c := range cells {
			if i == len(cells)-1 {
				b.WriteString(style(c))
				break
			}
			b.WriteString(st.column(style(c), widths[i]+2))
		}
		printf(w, "%s\n", strings.TrimRight(b.String(), " "))
	}
	line(header, st.dim)
	for _, row := range rows {
		line(row, func(s string) string { return s })
	}
}
