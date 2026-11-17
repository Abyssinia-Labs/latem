package main

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
)

// likeMachine is what `latem setup --like` takes from another machine's
// config.toml (A102): the actions it allows, the labels it aliases, and the
// two settings that give something up. Nothing else: its repositories,
// runners and control plane are its own.
type likeMachine struct {
	// name is as the person gave it, and how says where it was read.
	name, how string

	allowed         []string
	aliases         map[string][]string
	allowUnisolated bool
	requirePinned   bool
}

// likeSSHTimeout bounds the connection to the other machine, so that a host
// that does not answer stops setup before it writes anything.
const likeSSHTimeout = 15 * time.Second

// likeIsFile says whether --like names a file on this machine rather than a
// machine ssh reaches. A host has no slash in it, and a file passed here is
// a config.toml.
func likeIsFile(like string) bool {
	return strings.ContainsRune(like, '/') || strings.HasSuffix(like, ".toml")
}

// remoteConfigCommand prints the other machine's config.toml, wherever its
// latem reads it from by default. It runs no latem, so the other machine's
// latem may be of any version.
func remoteConfigCommand() string {
	return `cat "${` + config.EnvConfigDir + `:-$HOME/` + config.DirName + `}/` + config.FileName + `"`
}

// readLikeMachine reads the config.toml --like names, over ssh or from a file.
// It is read before setup writes anything, so a machine it cannot reach
// leaves this one as it was.
func readLikeMachine(ctx context.Context, like string) (likeMachine, error) {
	var (
		body []byte
		how  string
	)
	if likeIsFile(like) {
		b, err := os.ReadFile(expandHome(like))
		if err != nil {
			return likeMachine{}, fmt.Errorf("--like %s: %w", like, err)
		}
		body, how = b, "from "+like
	} else {
		ctx, cancel := context.WithTimeout(ctx, likeSSHTimeout+5*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		argv := []string{"ssh", "-o", fmt.Sprintf("ConnectTimeout=%d", int(likeSSHTimeout.Seconds())),
			"--", like, remoteConfigCommand()}
		if err := runSSH(ctx, argv, nil, &stdout, &stderr); err != nil {
			why := strings.TrimSpace(stderr.String())
			if why == "" {
				why = err.Error()
			}
			return likeMachine{}, fmt.Errorf("--like %s: could not read its config.toml over ssh: %s\n\n"+
				"nothing was written. `ssh %s` has to work from this machine, or pass its config.toml as a file: "+
				"--like ./%s.toml", like, displayText(why), like, like)
		}
		body, how = stdout.Bytes(), "over ssh"
	}
	m, err := parseLikeMachine(body)
	if err != nil {
		return likeMachine{}, fmt.Errorf("--like %s: its config.toml does not read: %w\n\nnothing was written", like, err)
	}
	m.name, m.how = like, how
	return m, nil
}

// parseLikeMachine reads only the parts --like copies. The rest of the file
// is the other machine's: its paths and runners need not make sense here, so
// it is not loaded as a config, and the edits are checked by editConfig's
// load gate on this machine instead.
func parseLikeMachine(body []byte) (likeMachine, error) {
	var file struct {
		Actions      config.Actions      `toml:"actions"`
		LabelAliases map[string][]string `toml:"label_aliases"`
	}
	if _, err := toml.Decode(string(body), &file); err != nil {
		return likeMachine{}, err
	}
	return likeMachine{
		allowed:         file.Actions.Allowed,
		aliases:         file.LabelAliases,
		allowUnisolated: file.Actions.AllowUnisolated,
		requirePinned:   file.Actions.PinningRequired(),
	}, nil
}

// likeOutcome is what --like left for a person, to say again at the end.
type likeOutcome struct {
	wrote bool
	// held is each line a person at a terminal has to agree to, when nobody
	// could be asked.
	held []string
}

// setupLike makes this machine's config.toml like m's (A102). The actions m
// allows are written: an action is still refused unless a job here names it,
// and is the same code on both machines. The label aliases and the two
// settings are a person's to agree to at a terminal (A100, A94), each shown
// as it will be written; off one, they are left and said.
func setupLike(cmd *cobra.Command, p *prompter, m likeMachine, cfg *config.Config) (likeOutcome, error) {
	out := cmd.OutOrStdout()
	paths := config.DerivePaths(configDir())
	var res likeOutcome
	printf(out, "  like %s: its config.toml, read %s\n", m.name, m.how)

	// The allowlist: written, with no question.
	var adding []readiness.Change
	for _, a := range m.allowed {
		if !slices.Contains(cfg.Actions.Allowed, a) && !slices.ContainsFunc(adding, func(c readiness.Change) bool { return c.Value == a }) {
			adding = append(adding, readiness.Change{What: readiness.ChangeAllowed, Value: a})
		}
	}
	switch {
	case len(m.allowed) == 0:
		printf(out, "  [actions] allowed: %s allows none\n", m.name)
	case len(adding) == 0:
		printf(out, "  [actions] allowed: has all %d of %s's already\n", len(m.allowed), m.name)
	default:
		if err := applyChanges(adding); err != nil {
			return res, err
		}
		res.wrote = true
		names := make([]string, len(adding))
		for i, c := range adding {
			names[i] = c.Value
		}
		printf(out, "  [actions] allowed: added %s\n", strings.Join(names, ", "))
	}

	// The aliases: new labels only. A label this machine already aliases
	// keeps what it has here.
	var aliasLines []string
	for _, l := range slices.Sorted(maps.Keys(m.aliases)) {
		mine, ok := cfg.LabelAliases[l]
		switch {
		case !ok:
			aliasLines = append(aliasLines, fmt.Sprintf("[label_aliases] %s = %s", tomlKey(l), tomlStringList(m.aliases[l])))
		case !slices.Equal(mine, m.aliases[l]):
			printf(out, "  [label_aliases] %s: keeps %s here; %s has %s\n", tomlKey(l), tomlStringList(mine), m.name, tomlStringList(m.aliases[l]))
		}
	}

	// The two settings, where m gives up what this machine does not.
	type setting struct {
		line, warning string
		change        readiness.Change
	}
	var settings []setting
	if !m.requirePinned && cfg.Actions.PinningRequired() {
		settings = append(settings, setting{"[actions] require_pinned = false", readiness.WarnMovingRefs,
			readiness.Change{What: readiness.ChangeMovingRefs}})
	}
	if m.allowUnisolated && !cfg.Actions.AllowUnisolated {
		settings = append(settings, setting{"[actions] allow_unisolated = true", readiness.WarnUnisolated,
			readiness.Change{What: readiness.ChangeUnisolated}})
	}
	if len(aliasLines) == 0 && len(settings) == 0 {
		return res, nil
	}

	in, _, notOne := terminalPair(cmd)
	if notOne != "" {
		res.held = append(res.held, aliasLines...)
		for _, s := range settings {
			res.held = append(res.held, s.line)
		}
		printf(out, "  left for a person at a terminal, because %s: %s's label aliases and the settings "+
			"that give something up are agreed to there (A102):\n", notOne, m.name)
		for _, l := range res.held {
			printf(out, "    %s\n", l)
		}
		printf(out, "  they type, at a terminal on this machine:\n    %s\n", likeLine(m.name))
		return res, nil
	}
	ask := func(warning string, lines []string) (bool, error) {
		printf(out, "\n  %s\n", warning)
		for _, l := range lines {
			printf(out, "    %s\n", l)
		}
		// An answer is one typed after the question was on the screen,
		// and setup's own reader may hold a line typed before it.
		if err := discardTypeAhead(in); err != nil {
			return false, fmt.Errorf("clearing what was typed ahead: %w", err)
		}
		p.in.Reset(in)
		return p.confirm("  Write this to config.toml?", false)
	}

	if len(aliasLines) > 0 {
		yes, err := ask(fmt.Sprintf("%s's label aliases. %s", m.name, likeAliasWarning), aliasLines)
		if err != nil {
			return res, err
		}
		if yes {
			if _, err := editConfig(paths, func(body string) (string, error) {
				for _, l := range slices.Sorted(maps.Keys(m.aliases)) {
					if _, ok := cfg.LabelAliases[l]; ok {
						continue
					}
					var err error
					if body, err = setTableKey(body, []string{"label_aliases"}, []string{"label_aliases"}, l, tomlStringList(m.aliases[l])); err != nil {
						return "", err
					}
				}
				return body, nil
			}); err != nil {
				return res, err
			}
			res.wrote = true
			printf(out, "  added %d label alias%s\n", len(aliasLines), map[bool]string{true: "", false: "es"}[len(aliasLines) == 1])
		} else {
			printf(out, "  left the label aliases out\n")
		}
	}
	for _, s := range settings {
		yes, err := ask(fmt.Sprintf("As on %s: %s", m.name, s.warning), []string{s.line})
		if err != nil {
			return res, err
		}
		if !yes {
			printf(out, "  left %s out\n", s.line)
			continue
		}
		if err := applyChanges([]readiness.Change{s.change}); err != nil {
			return res, err
		}
		res.wrote = true
		printf(out, "  wrote %s\n", s.line)
	}
	return res, nil
}

// likeAliasWarning is said before an alias --like copies is written. It is
// why an alias is the person's to agree to (A100).
const likeAliasWarning = "An alias sends this machine every job of its repositories that asks for the label, " +
	"and tells the control plane to send them here."

// likeLine is the command a person types at a terminal to finish what --like
// left: setup again, which writes nothing twice and asks only what is left.
func likeLine(like string) string {
	line := "latem setup --like " + shellWord(like)
	if global.configDir != "" {
		line += " --config-dir " + shellWord(global.configDir)
	}
	return line
}
