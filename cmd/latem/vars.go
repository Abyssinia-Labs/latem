package main

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
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

These commands edit those tables as text, keeping the rest of config.toml as
it was, then ask the running agent to reload.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newVarsSetCommand(), newVarsListCommand(), newVarsRmCommand())
	return cmd
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
			var rows [][]string
			for _, key := range slices.Sorted(maps.Keys(cfg.Repos)) {
				if sc.repo != "" && key != sc.repo {
					continue
				}
				r := cfg.Repos[key]
				if sc.env == "" {
					for _, n := range slices.Sorted(maps.Keys(r.Vars)) {
						rows = append(rows, []string{key, "-", n, r.Vars[n]})
					}
				}
				for _, env := range slices.Sorted(maps.Keys(r.Environments)) {
					if sc.env != "" && env != sc.env {
						continue
					}
					vars := r.Environments[env].Vars
					for _, n := range slices.Sorted(maps.Keys(vars)) {
						rows = append(rows, []string{key, env, n, vars[n]})
					}
				}
			}
			if len(rows) == 0 {
				printf(cmd.ErrOrStderr(), "no variables set for %s — `latem vars set --repo R NAME VALUE` sets one\n", sc)
				return nil
			}
			printTable(cmd.OutOrStdout(), []string{"REPO", "ENVIRONMENT", "NAME", "VALUE"}, rows)
			return nil
		},
	}
	scope.register(cmd)
	return cmd
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
