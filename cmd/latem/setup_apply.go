package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
)

// `latem setup apply` (A94): what the setup screen does to close an item, for
// a caller with a shell and no screen, which is an agent setting a machine up.
//
// It is the screen's code without the screen. An item is found by its id in a
// plan built afresh. Its commands run as runItem runs them for the screen
// (A60), its config.toml changes go through applyChanges and editConfig's
// load gate, and a kind goes on with its label only once proveKind passes.
// What is a person's stays a person's, and says so with exit 4 (A89).

type setupApplyFlags struct {
	kind string
	json bool
}

func newSetupApplyCommand() *cobra.Command {
	var f setupApplyFlags
	cmd := &cobra.Command{
		Use:   "apply <item-id>... | --kind K",
		Short: "Close what `latem setup --json` says is in the way, without the screen",
		Long: `Close what ` + "`latem setup --json`" + ` says stands in the way of a kind of build,
as the setup screen would, without the screen: for an agent with a shell on
this machine, or a script.

Each id is an item's, from the plan. --kind K is every item of the kind K,
and then the kind itself: once nothing is in the way, its check runs in a
step's environment, and the runner gains the kind's label only if the check
passes. With ids as well, --kind K only says to find them in K, since an id
can be in more than one kind, and K is not switched on: --kind K alone does
that, once its items are closed.

An item closes as the screen would close it:
  run      each command, as an argv, with stdin closed and in a session of
           its own, so nothing it starts can ask for a password. A program
           that is not installed is said and not run. Never sudo.
  config   the change, written to config.toml through the same edit and the
           same check that the file still loads. The running agent is then
           reloaded, or restarted once nothing is running when only a restart
           applies the change.
  yours    refused: commands for the person who runs this machine to type,
  manual   or words for them. latem never runs sudo, accepts a licence or
           installs Xcode.

Two settings give something up, and only a person at a terminal may write
them: unisolated ([actions] allow_unisolated = true) and moving-refs
([actions] require_pinned = false). At a terminal it asks first, and the
answer defaults to no. Anywhere else it refuses.

Each item gets a line on stderr: what ran, what was written, what is left.
--json prints the plan afterwards on stdout, as ` + "`latem setup --json`" + ` does.

Exit codes:
  0  every item named was closed
  1  one was not: a command failed, a check failed, or a person said no;
     what came after it was not tried
  2  usage: an id or a kind this machine's plan does not have
  4  one is a person's to do, and the lines above say what to ask them`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && f.kind == "" {
				return usageErrorf("name an item by its id, or a kind with --kind; `latem setup --json` lists both")
			}
			return runSetupApply(cmd, args, f, newSetupDeps())
		},
	}
	cmd.Flags().StringVar(&f.kind, "kind", "", "alone: every item of the kind `K`, then K itself, on with its label once its check passes; with ids: find them in K")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the plan afterwards, as setup --json does")
	return cmd
}

// applyTarget is one thing apply was asked to close: an item, or with on, a
// kind's switch.
type applyTarget struct {
	id string // the item's, or with on the kind's name
	// kind is where to look for it; "" is any kind.
	kind string
	on   bool
}

// applier holds what apply has done so far, and the plan as it stands.
type applier struct {
	ctx  context.Context
	cmd  *cobra.Command
	w    io.Writer
	view *machineView
	deps setupDeps
	plan readiness.Plan

	wrote   bool     // config.toml changed
	touched []string // the kinds an item was found in, for what is left
	failed  []string // ids, each with why
	persons []string // ids that are a person's
}

func runSetupApply(cmd *cobra.Command, ids []string, f setupApplyFlags, deps setupDeps) error {
	paths := config.DerivePaths(configDir())
	if _, err := os.Stat(paths.File); err != nil {
		return usageErrorf("this machine has no config yet; run `latem setup` first")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	m, err := openMachineView(ctx, cfg, deps.toolchains, fromMirrorThenRemote)
	if err != nil {
		return err
	}
	defer m.close()
	plan, err := m.plan(ctx)
	if err != nil {
		return err
	}
	targets, err := applyTargets(plan, ids, f.kind)
	if err != nil {
		return err
	}

	a := &applier{ctx: ctx, cmd: cmd, w: cmd.ErrOrStderr(), view: m, deps: deps, plan: plan}
	for i, t := range targets {
		if len(a.failed) > 0 {
			// What failed may be what the rest needs: the plan puts a JDK
			// before what runs on it.
			for _, rest := range targets[i:] {
				printf(a.w, "%s: not tried, after the failure above\n", rest.id)
			}
			break
		}
		if t.on {
			a.switchOn(t)
		} else {
			a.item(t)
		}
	}
	if a.wrote {
		a.tellAgent(paths)
	}
	a.left()
	if f.json {
		if err := writePlan(cmd.OutOrStdout(), a.plan); err != nil {
			return err
		}
	}
	return a.verdict()
}

// applyTargets reads the command line against the plan: every id named must
// be an item's in it, and --kind a kind's.
func applyTargets(plan readiness.Plan, ids []string, kind string) ([]applyTarget, error) {
	var scope *readiness.Kind
	if kind != "" {
		i := slices.IndexFunc(plan.Kinds, func(k readiness.Kind) bool { return strings.EqualFold(k.Name, kind) })
		if i < 0 {
			names := make([]string, len(plan.Kinds))
			for n, k := range plan.Kinds {
				names[n] = k.Name
			}
			return nil, usageErrorf("--kind %s: this machine's plan has no such kind. It has %s", kind, strings.Join(names, ", "))
		}
		scope = &plan.Kinds[i]
	}
	where := ""
	if scope != nil {
		where = scope.Name
	}
	var out []applyTarget
	for _, id := range ids {
		if slices.ContainsFunc(out, func(t applyTarget) bool { return t.id == id }) {
			continue
		}
		if len(itemsWith(plan, id, where)) == 0 {
			return nil, usageErrorf("%s is not an item here. %s", shellWord(id), idsThereAre(plan, scope))
		}
		out = append(out, applyTarget{id: id, kind: where})
	}
	// --kind alone is the kind: its items, then its switch. With ids it only
	// says where to find them, since one id can be in several kinds (a
	// repository's secrets are in each kind its jobs are), and switching the
	// kind on is left for --kind alone. An agent closing one item of several
	// should not be told the kind failed because the others are still there.
	if scope != nil && len(ids) == 0 {
		for _, it := range scope.Items {
			out = append(out, applyTarget{id: it.ID, kind: where})
		}
		out = append(out, applyTarget{id: scope.Name, kind: where, on: true})
	}
	return out, nil
}

// idsThereAre lists the items of scope, or of every kind, as a usage error
// names them.
func idsThereAre(plan readiness.Plan, scope *readiness.Kind) string {
	kinds := plan.Kinds
	if scope != nil {
		kinds = []readiness.Kind{*scope}
	}
	var parts []string
	for _, k := range kinds {
		if len(k.Items) == 0 {
			continue
		}
		ids := make([]string, len(k.Items))
		for i, it := range k.Items {
			ids[i] = shellWord(it.ID)
		}
		parts = append(parts, k.Name+": "+strings.Join(ids, ", "))
	}
	switch {
	case len(parts) > 0:
		return "The ids there are, by kind: " + strings.Join(parts, "; ")
	case scope != nil:
		return "Nothing is in the way of " + scope.Title + "."
	}
	return "Nothing is in the way of any kind here."
}

// found is an item, and the kind it was found in.
type found struct {
	kind readiness.Kind
	item readiness.Item
}

// itemsWith finds the item id in kind, or in every kind when kind is "". One
// item can be in the way of several kinds: a native runner is, of each kind
// that wants one.
func itemsWith(plan readiness.Plan, id, kind string) []found {
	var out []found
	for _, k := range plan.Kinds {
		if kind != "" && k.Name != kind {
			continue
		}
		for _, it := range k.Items {
			if it.ID == id {
				out = append(out, found{kind: k, item: it})
			}
		}
	}
	return out
}

func (a *applier) touch(kind string) {
	if !slices.Contains(a.touched, kind) {
		a.touched = append(a.touched, kind)
	}
}

func (a *applier) fail(id, why string) {
	printf(a.w, "%s: %s\n", id, why)
	a.failed = append(a.failed, shellWord(id)+": "+why)
}

// item closes one item as the screen's act would: its commands, else a
// person's, else its config changes.
func (a *applier) item(t applyTarget) {
	at := itemsWith(a.plan, t.id, t.kind)
	if len(at) == 0 {
		printf(a.w, "%s: closed already, by what came before it\n", t.id)
		return
	}
	k, it := at[0].kind, at[0].item
	for _, f := range at {
		a.touch(f.kind.Name)
	}
	switch {
	case len(it.Run) > 0:
		wrote, err := runItem(a.ctx, a.w, it, k.Runner, a.deps.toolchains)
		a.wrote = a.wrote || wrote
		ran := "ran " + strings.Join(commandLines(it.Run), "; ")
		if err != nil {
			a.look()
			a.fail(t.id, fmt.Sprintf("%s, which failed: %v", ran, err))
			return
		}
		a.closedBy(t, ran)
	case len(it.Yours) > 0 || len(it.Config) == 0:
		a.person(t.id, at)
	default:
		if givesUp(it.Config) && !a.personSaysYes(t, it) {
			return
		}
		if err := a.change(k, it.Config); err != nil {
			a.fail(t.id, err.Error())
			return
		}
		a.closedBy(t, "wrote "+strings.Join(changeLines(it.Config), "; "))
	}
}

// closedBy looks again and says whether what was done closed the item.
func (a *applier) closedBy(t applyTarget, did string) {
	if !a.look() {
		a.fail(t.id, did+", and the machine could not be looked at again")
		return
	}
	at := itemsWith(a.plan, t.id, t.kind)
	switch {
	case len(at) == 0:
		printf(a.w, "%s: %s; closed\n", t.id, did)
	case len(at[0].item.Run) == 0 && len(at[0].item.Config) == 0:
		printf(a.w, "%s: %s\n", t.id, did)
		a.person(t.id, at)
	default:
		still := "it is still in the way"
		if h := at[0].item.Have; h != "" {
			still += ": this machine has " + h
		}
		a.fail(t.id, did+", and "+still)
	}
}

// look builds the plan again, as the screen does after each thing it does.
func (a *applier) look() bool {
	plan, err := a.view.plan(a.ctx)
	if err != nil {
		printf(a.w, "the machine could not be looked at again: %v\n", err)
		return false
	}
	a.plan = plan
	return true
}

// person says what of an item is a person's, and what they would type. The
// item can be in the way of several kinds, each with its own lines: a
// repository's secrets are, one kind's jobs reading some and another's others.
func (a *applier) person(id string, at []found) {
	var lines, words []string
	for _, f := range at {
		for _, l := range commandLines(f.item.Yours) {
			if !slices.Contains(lines, l) {
				lines = append(lines, l)
			}
		}
		for _, m := range f.item.Manual {
			if !slices.Contains(words, m) {
				words = append(words, m)
			}
		}
	}
	switch {
	case len(lines) > 0:
		printf(a.w, "%s: a person's to type, on this machine; latem never runs sudo, accepts a licence or sets a secret's value:\n", id)
	case len(words) > 0:
		printf(a.w, "%s: a person's to do:\n", id)
	default:
		printf(a.w, "%s: a person's to do, and the plan says nothing more about it\n", id)
	}
	for _, l := range append(lines, words...) {
		printf(a.w, "  %s\n", l)
	}
	a.persons = append(a.persons, shellWord(id))
}

// givesUp reports whether changes hold one of the two settings that widen
// what may run here, which only a person at a terminal may write.
func givesUp(changes []readiness.Change) bool {
	return slices.ContainsFunc(changes, func(c readiness.Change) bool {
		return c.What == readiness.ChangeUnisolated || c.What == readiness.ChangeMovingRefs
	})
}

// personSaysYes asks the person at the terminal, as the screen does, and
// refuses when nobody can be: stdin or stdout is not a terminal (A89).
func (a *applier) personSaysYes(t applyTarget, it readiness.Item) bool {
	in, out, notOne := terminalPair(a.cmd)
	if notOne != "" {
		printf(a.w, "%s: it gives something up, so only a person at a terminal may write it, and %s. %s\n",
			t.id, notOne, it.Warning)
		for _, l := range changeLines(it.Config) {
			printf(a.w, "  it writes %s\n", l)
		}
		printf(a.w, "  the person who runs this machine types, at a terminal on it:\n  %s\n", applyLine(t))
		a.persons = append(a.persons, shellWord(t.id))
		return false
	}
	printf(out, "%s\n", it.Title)
	if it.Warning != "" {
		printf(out, "%s\n", it.Warning)
	}
	for _, l := range indented(changeLines(it.Config)) {
		printf(out, "%s\n", l)
	}
	// An answer is one typed after the question was on the screen.
	if err := discardTypeAhead(in); err != nil {
		a.fail(t.id, fmt.Sprintf("clearing what was typed ahead: %v", err))
		return false
	}
	yes, err := (&prompter{in: bufio.NewReader(in), out: out}).confirm("Write this to config.toml?", false)
	switch {
	case err != nil:
		a.fail(t.id, err.Error())
	case !yes:
		a.fail(t.id, "left as it is: the answer was no")
	}
	return err == nil && yes
}

// applyLine is the command a person types to write t at a terminal.
func applyLine(t applyTarget) string {
	line := "latem setup apply " + shellWord(t.id)
	if t.kind != "" {
		line += " --kind " + t.kind
	}
	if global.configDir != "" {
		line += " --config-dir " + shellWord(global.configDir)
	}
	return line
}

// change writes changes to config.toml as the screen's apply hook does. A
// label goes on only once the kind's proof has passed in a step's
// environment, so a kind that does not work is never sent jobs.
func (a *applier) change(k readiness.Kind, changes []readiness.Change) error {
	if slices.ContainsFunc(changes, func(c readiness.Change) bool { return c.What == readiness.ChangeLabelAdd }) {
		if err := proveKind(a.ctx, a.view.stepPath(k.Runner), k); err != nil {
			a.proofOutput(err)
			return fmt.Errorf("the check failed, so no label was written: %w", err)
		}
	}
	if err := applyChanges(changes); err != nil {
		return fmt.Errorf("config.toml was left as it was: %w", err)
	}
	a.wrote = true
	return nil
}

// proofOutput shows what a failed probe printed: the error says its first
// line, and a person reading why wants the rest.
func (a *applier) proofOutput(err error) {
	var pf *proofFailed
	if !errors.As(err, &pf) || pf.output == "" {
		return
	}
	lines := strings.Split(pf.output, "\n")
	if len(lines) > 20 {
		lines = append(lines[:20], fmt.Sprintf("… and %d more lines", len(lines)-20))
	}
	printf(a.w, "%s printed:\n", pf.title)
	for _, l := range lines {
		printf(a.w, "  %s\n", displayText(l))
	}
}

// switchOn turns a kind on as the screen's space bar does: its label on its
// runner, with nothing in the way and its proof passed.
func (a *applier) switchOn(t applyTarget) {
	a.touch(t.id)
	i := slices.IndexFunc(a.plan.Kinds, func(k readiness.Kind) bool { return k.Name == t.id })
	if i < 0 {
		a.fail(t.id, "it is no longer in this machine's plan")
		return
	}
	k := a.plan.Kinds[i]
	switch {
	case k.State == readiness.Impossible:
		a.fail(t.id, fmt.Sprintf("%s %s, and nothing here changes that", k.Title, k.Why))
	case k.On:
		printf(a.w, "%s: on, on the %s runner\n", t.id, k.Runner)
	case k.Label == "":
		printf(a.w, "%s: a person's to do: %s comes with the %s runner's labels, and which labels a runner carries is said in config.toml\n",
			t.id, k.Title, orDash(k.Runner))
		a.persons = append(a.persons, t.id)
	case k.Blocking() > 0:
		var ids []string
		theirs := true
		for _, it := range k.Items {
			if !it.Soft {
				ids = append(ids, shellWord(it.ID))
				theirs = theirs && len(it.Run) == 0 && (len(it.Yours) > 0 || len(it.Config) == 0 || givesUp(it.Config))
			}
		}
		why := fmt.Sprintf("off: %d to do before this machine takes %s jobs: %s", len(ids), k.Title, strings.Join(ids, ", "))
		if theirs {
			printf(a.w, "%s: %s, each a person's\n", t.id, why)
			a.persons = append(a.persons, t.id)
			return
		}
		a.fail(t.id, why)
	default:
		change := kindLabel(k, readiness.ChangeLabelAdd)
		if err := a.change(k, []readiness.Change{change}); err != nil {
			a.fail(t.id, err.Error())
			return
		}
		titles := make([]string, len(k.Proof))
		for n, p := range k.Proof {
			titles[n] = p.Title
		}
		did := change.Line
		if len(titles) > 0 {
			did = "the check passed (" + strings.Join(titles, ", ") + "), and " + did
		}
		if !a.look() {
			a.fail(t.id, did+", and the machine could not be looked at again")
			return
		}
		if j := slices.IndexFunc(a.plan.Kinds, func(k readiness.Kind) bool { return k.Name == t.id }); j < 0 || !a.plan.Kinds[j].On {
			a.fail(t.id, did+", and it is still off")
			return
		}
		printf(a.w, "%s: %s; on\n", t.id, did)
	}
}

// tellAgent moves the running agent onto what was written, once for all of
// it: a reload, or a restart at its next idle moment for what only a restart
// applies (A74).
func (a *applier) tellAgent(paths config.Paths) {
	heard := tellAgent(paths)
	printf(a.w, "%s\n", heard.line)
	if len(heard.deferred) > 0 {
		printf(a.w, "  its reload left these for a restart: %s\n", strings.Join(heard.deferred, "; "))
	}
	if heard.none && a.deps.service != nil {
		if st, err := a.deps.service.Status(a.ctx); err == nil && !st.Installed {
			printf(a.w, "  no agent service is installed here; `latem service install` runs one, and it reads config.toml as it starts\n")
		}
	}
}

// left says, for each kind an item was found in, where it stands now and
// what is still in the way, each with how it closes.
func (a *applier) left() {
	for _, name := range a.touched {
		i := slices.IndexFunc(a.plan.Kinds, func(k readiness.Kind) bool { return k.Name == name })
		if i < 0 {
			continue
		}
		k := a.plan.Kinds[i]
		printf(a.w, "now %s is %s\n", k.Name, stateText(k))
		for _, it := range k.Items {
			soft := ""
			if it.Soft {
				soft = ", not counted"
			}
			printf(a.w, "  %s: %s (%s%s)\n", shellWord(it.ID), it.Title, closesBy(it), soft)
		}
	}
}

// closesBy names how an item closes, in the plan's own words for it.
func closesBy(it readiness.Item) string {
	switch {
	case len(it.Run) > 0:
		return "run"
	case len(it.Yours) > 0:
		return "yours"
	case len(it.Config) > 0 && givesUp(it.Config):
		return "config, a person's"
	case len(it.Config) > 0:
		return "config"
	}
	return "manual"
}

func (a *applier) verdict() error {
	switch {
	case len(a.failed) > 0:
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("not closed: %s", strings.Join(a.failed, "; "))}
	case len(a.persons) > 0:
		return &ExitError{Code: ExitNoTerminal, Err: fmt.Errorf(
			"%s %s for the person who runs this machine: stop, and ask them to do what is said above",
			strings.Join(a.persons, ", "), map[bool]string{true: "is", false: "are"}[len(a.persons) == 1])}
	}
	return nil
}
