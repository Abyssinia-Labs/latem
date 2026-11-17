// Package repovars answers what a workflow reads as ${{ vars.NAME }} (A50,
// A96): a repository's variables, from OpenVault when this machine reads it,
// and from config.toml.
//
// It is the one lookup every reader of variables uses, so that a run, a plan,
// a heartbeat's digests, `latem vars list` and `latem workflows check` cannot
// disagree about which value a job gets. The rule is §15's for secrets: most
// specific scope first, and within a scope OpenVault before config.toml,
// because the vault is the copy every machine shares and a machine's own copy
// is the one that goes stale.
package repovars

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// Where a variable's value came from.
const (
	FromOpenVault = "vault"
	FromConfig    = "config.toml"
)

// Vault is OpenVault as variables are read from it. A
// [*secrets.OpenVaultProvider] is one.
type Vault interface {
	Variables(ctx context.Context, collection string, envs func(env string) bool) (secrets.Variables, error)
}

// Set is one repository's variables, as read at one moment.
type Set struct {
	repo  *config.Repo
	vault secrets.Variables
}

// Read reads repository key's variables: OpenVault's, when vault is not nil,
// and config.toml's from repo, which may be nil.
//
// Only a variable entry of the vault answers, never a secret (see
// [secrets.OpenVaultProvider.Variables]). A collection this machine does not
// hold is no error: the vault has none of key's variables here, and
// config.toml answers. A service that does not answer is the error, never an
// empty set: a job must not read a variable as empty because its store was
// down.
func Read(ctx context.Context, vault Vault, key string, repo *config.Repo) (Set, error) {
	return ReadWhere(ctx, vault, key, repo, nil)
}

// ReadWhere is [Read] for the environments envs admits alone; the
// heartbeat reads only a repository's own variables (A68).
func ReadWhere(ctx context.Context, vault Vault, key string, repo *config.Repo, envs func(string) bool) (Set, error) {
	s := Set{repo: repo}
	if vault == nil {
		return s, nil
	}
	v, err := vault.Variables(ctx, key, envs)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return s, nil
	case err != nil:
		return s, fmt.Errorf("reading %s's variables from the vault: %w", key, err)
	}
	s.vault = v
	return s, nil
}

// Of is a set already read: config.toml's from repo, and vault's as read.
func Of(repo *config.Repo, vault secrets.Variables) Set { return Set{repo: repo, vault: vault} }

// For is the vars context of a job in environment env, "" for none: the
// repository's variables, OpenVault's over config.toml's, and then the
// environment's over them, OpenVault's over config.toml's again.
func (s Set) For(env string) map[string]string {
	out := map[string]string{}
	for _, l := range s.layers(env) {
		maps.Copy(out, l.vars)
	}
	return out
}

// Var is one variable a scope holds, and where.
type Var struct {
	// Env is its environment, "" for the repository's own.
	Env   string
	Name  string
	Value string
	// Sources are the stores that hold it in this scope, the one that
	// answers first.
	Sources []string
}

// List is every variable the set holds, by scope and name, the repository's
// own first, for `latem vars list`.
func (s Set) List() []Var {
	envs := map[string]bool{"": true}
	if s.repo != nil {
		for e := range s.repo.Environments {
			envs[e] = true
		}
	}
	for e := range s.vault {
		envs[e] = true
	}
	var out []Var
	for _, env := range slices.Sorted(maps.Keys(envs)) {
		byName := map[string]*Var{}
		for _, l := range s.scope(env) {
			for name, value := range l.vars {
				v := byName[name]
				if v == nil {
					v = &Var{Env: env, Name: name, Value: value}
					byName[name] = v
				}
				v.Sources = append(v.Sources, l.source)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(byName)) {
			out = append(out, *byName[name])
		}
	}
	return out
}

// Holds reports whether name is set for a job in env, by any store.
func (s Set) Holds(name, env string) bool {
	_, ok := s.For(env)[name]
	return ok
}

// Source is the store whose value a job in env reads for name, or "".
func (s Set) Source(name, env string) string {
	source := ""
	for _, l := range s.layers(env) {
		if _, ok := l.vars[name]; ok {
			source = l.source
		}
	}
	return source
}

type layer struct {
	source string
	vars   map[string]string
}

// layers are what For merges, the one that wins last.
func (s Set) layers(env string) []layer {
	out := slices.Clone(s.scope(""))
	slices.Reverse(out)
	if env != "" {
		scoped := slices.Clone(s.scope(env))
		slices.Reverse(scoped)
		out = append(out, scoped...)
	}
	return out
}

// scope is one scope's stores, in the order they answer.
func (s Set) scope(env string) []layer {
	var out []layer
	if v := s.vault[env]; len(v) > 0 {
		out = append(out, layer{FromOpenVault, v})
	}
	if s.repo != nil {
		var c map[string]string
		if env == "" {
			c = s.repo.Vars
		} else if e, ok := s.repo.Environments[env]; ok {
			c = e.Vars
		}
		if len(c) > 0 {
			out = append(out, layer{FromConfig, c})
		}
	}
	return out
}
