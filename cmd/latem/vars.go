package main

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/repovars"
)

func newVarsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vars",
		Short: "Set, list and remove the variables a repository's workflows read",
		Long: `Set, list and remove the variables a repository's workflows read.

A workflow reads ${{ vars.NAME }} from [repos.<key>.vars] in config.toml,
and a job in an environment from [repos.<key>.environments.<env>.vars] over
them (A50). Variables are not secrets: they may appear in a compiled job and
in its log. For anything that may not, use ` + "`latem secrets`" + `.

With [secrets] vault = true, the vault comes first (A96): a variable
entry of collection <key>, <env>/NAME for an environment and NAME for the
repository, each before config.toml's in its scope. ` + "`latem-vault set`" + `
<key>/<env>/NAME --var writes one there, and list shows which answers.

set and rm edit config.toml as text, keeping the rest of it as it was, then
ask the running agent to reload. latem never writes to the vault.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newVarsSetCommand(), newVarsListCommand(), newVarsRmCommand())
	return cmd
}

// readRepoVars is repository key's variables as a run reads them (A96):
// OpenVault's first, when the config reads it, then config.toml's. A vault
// that does not answer is the error, with config.toml's alone in the set.
func readRepoVars(ctx context.Context, cfg *config.Config, key string) (repovars.Set, error) {
	stores, err := varStores(cfg)
	if err != nil {
		return repovars.Of(cfg.Repos[key], nil), err
	}
	return repoVarsIn(ctx, cfg, stores, checkTarget{repoKey: key, repo: cfg.Repos[key]})
}

// varStores are the stores variables are read from: none but config.toml's
// unless the config reads OpenVault.
func varStores(cfg *config.Config) (secretStores, error) {
	if !cfg.Secrets.Vault {
		return secretStores{}, nil
	}
	return newSecretStores(cfg.Paths)
}

// repoVarsIn is [readRepoVars] from stores already made, for a check target,
// which may be a checkout this machine has no [repos.*] entry for.
func repoVarsIn(ctx context.Context, cfg *config.Config, stores secretStores, t checkTarget) (repovars.Set, error) {
	if t.repo == nil {
		return repovars.Of(nil, nil), nil
	}
	var vault repovars.Vault
	if cfg.Secrets.Vault && stores.openvault != nil {
		vault = stores.openvault
	}
	set, err := repovars.Read(ctx, vault, t.repoKey, t.repo)
	if err != nil {
		return repovars.Of(t.repo, nil), err
	}
	return set, nil
}

// varName is GitHub's rule for a variable's name, which internal/config
// enforces at load; checked here first so the refusal names the variable
// rather than a line of config.toml.
var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func checkVarName(name string) error {
	switch {
	case !varName.MatchString(name):
		return usageErrorf("%q is not a variable name; it must match %s", name, varName)
	case strings.HasPrefix(strings.ToUpper(name), "GITHUB_"):
		return usageErrorf("%s: names starting GITHUB_ are GitHub's own", name)
	}
	return nil
}

// varsTable is the table a scope's variables live in.
func varsTable(sc secretScope) []string {
	if sc.env == "" {
		return []string{"repos", sc.repo, "vars"}
	}
	return []string{"repos", sc.repo, "environments", sc.env, "vars"}
}

// varsScope resolves --repo and --env, which vars require: there are no
// machine-wide variables.
func varsScope(f scopeFlags) (secretScope, error) {
	if f.repo == "" {
		return secretScope{}, usageErrorf("--repo is required: variables belong to a repository")
	}
	return f.resolve()
}

func newVarsSetCommand() *cobra.Command {
	var scope scopeFlags
	cmd := &cobra.Command{
		Use:   "set --repo R [--env E] NAME VALUE",
		Short: "Set a variable",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := varsScope(scope)
			if err != nil {
				return err
			}
			name, value := args[0], args[1]
			if err := checkVarName(name); err != nil {
				return err
			}
			if err := checkControl("vars."+name, []byte(value)); err != nil {
				return usageErrorf("%v", err)
			}
			paths := config.DerivePaths(configDir())
			warning, err := editConfig(paths, func(body string) (string, error) {
				return setTableKey(body, varsTable(sc), []string{"repos", sc.repo}, name, tomlString(value))
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printf(out, "set vars.%s for %s\n", name, sc)
			if shadow := openVaultVarShadow(cmd.Context(), sc, name); shadow != "" {
				printf(out, "\n%s\n", shadow)
			}
			if warning != "" {
				printf(out, "\n%s\n", warning)
			}
			printf(out, "\n")
			announceConfigChange(out, paths)
			return nil
		},
	}
	scope.register(cmd)
	return cmd
}

func newVarsListCommand() *cobra.Command {
	var scope scopeFlags
	cmd := &cobra.Command{
		Use:   "list [--repo R] [--env E]",
		Short: "List variables and their values",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc, err := scope.resolve()
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			stores, err := varStores(cfg)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, key := range slices.Sorted(maps.Keys(cfg.Repos)) {
				if sc.repo != "" && key != sc.repo {
					continue
				}
				set, err := repoVarsIn(cmd.Context(), cfg, stores, checkTarget{repoKey: key, repo: cfg.Repos[key]})
				if err != nil {
					// config.toml's are still listed: they are this machine's
					// own, and the vault's are what a job would fail on now.
					printf(cmd.ErrOrStderr(), "%v — the vault's variables are not listed, and a job that reads one fails until it answers\n", err)
				}
				for _, v := range set.List() {
					if sc.env != "" && v.Env != sc.env {
						continue
					}
					row := []string{key, orDash(v.Env), v.Name, v.Value}
					if cfg.Secrets.Vault {
						row = append(row, sourcesText(v.Sources))
					}
					rows = append(rows, row)
				}
			}
			if len(rows) == 0 {
				printf(cmd.ErrOrStderr(), "no variables set for %s — `latem vars set --repo R NAME VALUE` sets one\n", sc)
				return nil
			}
			header := []string{"REPO", "ENVIRONMENT", "NAME", "VALUE"}
			if cfg.Secrets.Vault {
				// Which store a job reads it from, only where there are two.
				header = append(header, "SOURCE")
			}
			printTable(cmd.OutOrStdout(), header, rows)
			return nil
		},
	}
	scope.register(cmd)
	return cmd
}

// sourcesText is where a variable is held, the one a job reads first named
// when there are two: "vault, config.toml (vault answers)".
func sourcesText(sources []string) string {
	if len(sources) < 2 {
		return strings.Join(sources, ", ")
	}
	return strings.Join(sources, ", ") + " (" + sources[0] + " answers)"
}

// openVaultVarShadow says when the vault holds the variable just set in
// config.toml, in the same scope: a job reads the vault's first (A96), so
// the value written is read only once the vault no longer holds one. Names
// only are compared.
func openVaultVarShadow(ctx context.Context, sc secretScope, name string) string {
	cfg, err := loadConfig()
	if err != nil || !cfg.Secrets.Vault {
		return ""
	}
	set, err := readRepoVars(ctx, cfg, sc.repo)
	if err != nil {
		return ""
	}
	for _, v := range set.List() {
		if v.Env == sc.env && v.Name == name && len(v.Sources) > 0 && v.Sources[0] == repovars.FromOpenVault {
			where := sc.repo + "/" + name
			if sc.env != "" {
				where = sc.repo + "/" + sc.env + "/" + name
			}
			return fmt.Sprintf("warning: the vault holds %s, and a job reads it before config.toml's; "+
				"this value is read only once the vault holds none (`%s set %s --var` changes the vault's)",
				where, vaultCommand(), where)
		}
	}
	return ""
}

func newVarsRmCommand() *cobra.Command {
	var scope scopeFlags
	cmd := &cobra.Command{
		Use:   "rm --repo R [--env E] NAME",
		Short: "Remove a variable",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := varsScope(scope)
			if err != nil {
				return err
			}
			name := args[0]
			if err := checkVarName(name); err != nil {
				return err
			}
			paths := config.DerivePaths(configDir())
			found := false
			warning, err := editConfig(paths, func(body string) (string, error) {
				updated, ok, err := removeTableKey(body, varsTable(sc), name)
				found = ok
				return updated, err
			})
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no vars.%s is set for %s in a [%s] table", name, sc, strings.Join(varsTable(sc), "."))
			}
			out := cmd.OutOrStdout()
			printf(out, "removed vars.%s for %s\n", name, sc)
			if warning != "" {
				printf(out, "\n%s\n", warning)
			}
			printf(out, "\n")
			announceConfigChange(out, paths)
			return nil
		},
	}
	scope.register(cmd)
	return cmd
}
