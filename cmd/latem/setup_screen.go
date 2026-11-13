package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
)

// The screen of `latem setup` (A69): what this machine builds, and what is in
// the way of each kind of build.
//
// It holds a plan and a cursor and nothing else. A key changes the cursor or
// asks a hook to do something, and view draws what is true now, so all of it
// is tested without a terminal. Every change to the machine goes through a
// hook and, but for a command the person types themselves, a yes.

// screenHooks are what the screen does to the world.
type screenHooks struct {
	// refresh looks at the machine again.
	refresh func() (readiness.Plan, error)
	// run runs an item's commands, and apply writes config.toml.
	run func(kind readiness.Kind, it readiness.Item) error
	// apply's string says, in a line, whether the running agent has it.
	apply func(changes []readiness.Change) (string, error)
	// shell gives the person a terminal, with lines to type in it.
	shell func(title string, lines []string) error
	// prove runs a kind's proof in a step's environment.
	prove func(k readiness.Kind) error
}

// screenAsk is a yes-or-no question the screen is waiting on.
type screenAsk struct {
	lines []string
	yes   func()
}

type setupScreen struct {
	hooks   screenHooks
	plan    readiness.Plan
	kind    int // the kind under the cursor
	item    int // the item under it, when inItems
	inItems bool
	status  string
	ask     *screenAsk
	done    bool
	st      styler
}

func newSetupScreen(plan readiness.Plan, hooks screenHooks, st styler) *setupScreen {
	return &setupScreen{plan: plan, hooks: hooks, st: st}
}

func (s *setupScreen) selected() (readiness.Kind, bool) {
	if s.kind < len(s.plan.Kinds) {
		return s.plan.Kinds[s.kind], true
	}
	return readiness.Kind{}, false
}

// key does what a key does.
func (s *setupScreen) key(k keyPress) {
	if k.code == keyInterrupt {
		s.done = true
		return
	}
	if ask := s.ask; ask != nil {
		s.ask = nil
		if k.code == keyRune && (k.r == 'y' || k.r == 'Y') {
			ask.yes()
		} else {
			s.status = "Left as it is."
		}
		return
	}
	s.status = ""
	kind, ok := s.selected()
	switch {
	case k.code == keyUp || k.r == 'k':
		s.move(-1)
	case k.code == keyDown || k.r == 'j':
		s.move(1)
	case k.code == keyLeft || k.code == keyEsc || k.r == 'h':
		s.inItems = false
	case k.r == 'q':
		s.done = true
	case k.r == 'r':
		s.reload("Looked again.")
	case k.r == 's':
		s.openShell("A shell here, for whatever you need to do.", nil)
	case !ok:
	case k.r == ' ':
		s.toggle(kind)
	case k.code == keyEnter || k.code == keyRight || k.r == 'l':
		switch {
		case !s.inItems && len(kind.Items) > 0:
			s.inItems, s.item = true, 0
		case s.inItems && k.code == keyEnter:
			s.act(kind, kind.Items[s.item])
		}
	}
}

func (s *setupScreen) move(by int) {
	kind, ok := s.selected()
	switch {
	case !ok:
	case s.inItems:
		s.item = min(max(s.item+by, 0), len(kind.Items)-1)
	default:
		s.kind = min(max(s.kind+by, 0), len(s.plan.Kinds)-1)
	}
}

// reload looks at the machine again and keeps the cursor on what it was on,
// or on what took the place of a thing just done.
func (s *setupScreen) reload(status string) {
	var kindName, itemID string
	if k, ok := s.selected(); ok {
		kindName = k.Name
		if s.inItems && s.item < len(k.Items) {
			itemID = k.Items[s.item].ID
		}
	}
	plan, err := s.hooks.refresh()
	if err != nil {
		s.status = "That looked wrong: " + err.Error()
		return
	}
	s.plan = plan
	s.status = status
	s.kind = max(slices.IndexFunc(plan.Kinds, func(k readiness.Kind) bool { return k.Name == kindName }), 0)
	k, ok := s.selected()
	if !ok {
		s.inItems = false
		return
	}
	if at := slices.IndexFunc(k.Items, func(it readiness.Item) bool { return it.ID == itemID }); at >= 0 {
		s.item = at
	} else {
		s.item = min(s.item, max(len(k.Items)-1, 0))
	}
	s.inItems = s.inItems && len(k.Items) > 0
}

func (s *setupScreen) act(k readiness.Kind, it readiness.Item) {
	switch {
	case len(it.Run) > 0:
		s.ask = &screenAsk{lines: append([]string{"Run this?"}, indented(commandLines(it.Run))...), yes: func() {
			if err := s.hooks.run(k, it); err != nil {
				s.reload(it.Title + ": " + err.Error())
				return
			}
			s.reload("Done: " + it.Title + ".")
		}}
	case len(it.Yours) > 0:
		s.openShell(yoursTitle(it), commandLines(it.Yours))
	case len(it.Config) > 0:
		lines := []string{"Write this to config.toml?"}
		if it.Warning != "" {
			lines = append(lines, it.Warning)
		}
		s.ask = &screenAsk{lines: append(lines, indented(changeLines(it.Config))...), yes: func() {
			s.change(it.Config, "Written to config.toml.")
		}}
	default:
		s.status = "This one is yours: what to do is on the screen."
	}
}

func (s *setupScreen) change(changes []readiness.Change, done string) {
	took, err := s.hooks.apply(changes)
	if err != nil {
		s.status = "config.toml was left as it was: " + err.Error()
		return
	}
	if took != "" {
		done += " " + took
	}
	s.reload(done)
}

func (s *setupScreen) openShell(title string, lines []string) {
	if err := s.hooks.shell(title, lines); err != nil {
		s.status = err.Error()
		return
	}
	s.reload("Looked again.")
}

// toggle switches a kind on or off, which is a label on a runner and nothing
// more. It goes on only with nothing in the way and the proof passed.
func (s *setupScreen) toggle(k readiness.Kind) {
	change := func(what string) {
		s.change([]readiness.Change{kindLabel(k, what)}, "Written to config.toml.")
	}
	switch {
	case k.State == readiness.Impossible:
		s.status = fmt.Sprintf("%s %s.", k.Title, k.Why)
	case k.Label == "":
		s.status = fmt.Sprintf("%s comes with the %s runner's labels; config.toml is where to change them.", k.Title, orDash(k.Runner))
	case k.Labelled:
		s.ask = &screenAsk{lines: []string{fmt.Sprintf("Stop taking %s jobs here? The %s runner loses the label %s.", k.Title, k.Runner, k.Label)},
			yes: func() { change(readiness.ChangeLabelDrop) }}
	case k.Blocking() > 0:
		s.status = fmt.Sprintf("%d to do before this machine can take %s jobs.", k.Blocking(), k.Title)
	default:
		if err := s.hooks.prove(k); err != nil {
			s.status = "The check failed, so the label stays off: " + err.Error()
			return
		}
		lines := []string{fmt.Sprintf("The tools work in a step's environment. Add the label %s to the %s runner?", k.Label, k.Runner)}
		s.ask = &screenAsk{lines: append(lines, k.Notes...), yes: func() { change(readiness.ChangeLabelAdd) }}
	}
}

// kindLabel is the change that switches k on or off: its label, on its
// runner. The screen's space bar and `latem setup apply --kind` write it.
func kindLabel(k readiness.Kind, what string) readiness.Change {
	verb := "gains"
	if what == readiness.ChangeLabelDrop {
		verb = "loses"
	}
	return readiness.Change{What: what, Runner: k.Runner, Value: k.Label,
		Line: fmt.Sprintf("the %s runner %s the label %s", k.Runner, verb, k.Label)}
}

func yoursTitle(it readiness.Item) string {
	return "This one is yours to run: latem never runs sudo or accepts a licence."
}

func commandLines(argvs [][]string) []string {
	out := make([]string, len(argvs))
	for i, a := range argvs {
		out[i] = needs.CommandLine(a)
	}
	return out
}

func changeLines(changes []readiness.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Line
	}
	return out
}

func indented(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = "  " + l
	}
	return out
}

// view draws the screen, at most height lines of at most width columns. The
// kinds stay put, what is under the cursor is always on it, and the last two
// lines are always what happened and what the keys do.
func (s *setupScreen) view(width, height int) []string {
	width, height = max(width, 20), max(height, 6)
	body, cursor := s.body(width)
	foot := s.foot(width)
	room := height - len(foot)
	if len(body) > room {
		// Cut from the bottom; scroll only as far as the cursor needs.
		offset := max(min(cursor-room+2, len(body)-room), 0)
		body = body[offset : offset+room]
	}
	for len(body) < room {
		body = append(body, "")
	}
	return append(body, foot...)
}

func (s *setupScreen) foot(width int) []string {
	keys := "↑↓ move · enter open · space on/off · r check again · s shell · q done"
	switch {
	case s.ask != nil:
		keys = "y yes · any other key no"
	case s.inItems:
		keys = "↑↓ move · enter do it · ← back · space on/off · r check again · s shell · q done"
	}
	return []string{s.st.warn(fit(s.status, width)), s.st.dim(fit(keys, width))}
}

func (s *setupScreen) body(width int) (lines []string, cursor int) {
	st := s.st
	host := s.plan.Host.Name
	if host == "" {
		host = s.plan.Host.OS
	}
	title := fmt.Sprintf("latem setup · %s · %s %s", s.plan.Machine, host, s.plan.Host.Arch)
	lines = append(lines, st.bold(fit(title, width)), "", st.bold("What this machine builds"), "")

	for i, k := range s.plan.Kinds {
		marker := "  "
		if i == s.kind {
			marker = "> "
			if !s.inItems {
				cursor = len(lines)
			}
		}
		check := "[ ]"
		if k.On {
			check = "[x]"
		}
		// Title, state and repositories, as much of each as the width has.
		titleWidth := min(18, width-7)
		stateWidth := min(22, width-7-titleWidth)
		row := fmt.Sprintf("%s%s %-*s ", marker, check, titleWidth, fit(k.Title, titleWidth))
		state := fmt.Sprintf("%-*s", stateWidth, fit(stateText(k), stateWidth))
		rest := fit(strings.Join(k.Repos, ", "), width-len([]rune(row))-stateWidth-1)
		lines = append(lines, row+s.paintState(k, state)+" "+st.dim(rest))
	}

	kind, ok := s.selected()
	if !ok {
		return lines, cursor
	}
	lines = append(lines, "", st.bold(fit(kindHeading(kind), width)))
	switch {
	case kind.State == readiness.Impossible:
		lines = append(lines, "  "+kind.Why)
	case len(kind.Items) == 0 && kind.On:
		lines = append(lines, "  Nothing is in the way.")
	case len(kind.Items) == 0:
		lines = append(lines, "  Nothing is in the way. Space turns it on.")
	}
	for i, it := range kind.Items {
		marker := "  "
		if s.inItems && i == s.item {
			marker = "> "
			cursor = len(lines)
		}
		sign := st.bad("✗")
		if it.Soft {
			sign = st.warn("~")
		}
		titleWidth := min(38, width-5)
		row := marker + sign + " " + fmt.Sprintf("%-*s", titleWidth, fit(it.Title, titleWidth))
		lines = append(lines, row+" "+st.dim(fit(it.Have, width-titleWidth-5)))
	}

	lines = append(lines, "")
	if s.ask != nil {
		for _, l := range s.ask.lines {
			lines = append(lines, wrapped(l, width)...)
		}
		cursor = len(lines) - 1
		return lines, cursor
	}
	if s.inItems && s.item < len(kind.Items) {
		lines = append(lines, itemDetail(kind.Items[s.item], width)...)
		return lines, cursor
	}
	if kind.Why != "" && kind.State != readiness.Impossible {
		lines = append(lines, wrapped(kind.Why, width)...)
	}
	for _, n := range kind.Notes {
		lines = append(lines, wrapped(n, width)...)
	}
	if len(s.plan.Unread) > 0 {
		lines = append(lines, wrapped("Not read: "+strings.Join(s.plan.Unread, "; "), width)...)
	}
	return lines, cursor
}

func kindHeading(k readiness.Kind) string {
	if k.Runner == "" || k.State == readiness.Impossible {
		return k.Title
	}
	return fmt.Sprintf("%s, on the %s runner", k.Title, k.Runner)
}

// itemDetail says what closes an item.
func itemDetail(it readiness.Item, width int) []string {
	var lines []string
	add := func(text string) { lines = append(lines, wrapped(text, width)...) }
	if len(it.Jobs) > 0 {
		add("For " + joinSome(it.Jobs, 3))
	}
	if len(it.Run) > 0 {
		add("latem runs this, once you say yes:")
		for _, l := range commandLines(it.Run) {
			add("  " + l)
		}
	}
	if len(it.Yours) > 0 {
		add("Yours to run. Enter opens a shell here, with this above the prompt:")
		lines = append(lines, firstLines(commandLines(it.Yours), width)...)
	}
	if len(it.Config) > 0 {
		add("Goes in config.toml, once you say yes:")
		for _, l := range changeLines(it.Config) {
			add("  " + l)
		}
	}
	if it.Warning != "" {
		add(it.Warning)
	}
	for _, m := range it.Manual {
		add(m)
	}
	return lines
}

// firstLines shows a long list by its start: nine secrets are nine commands.
func firstLines(cmds []string, width int) []string {
	var out []string
	for i, l := range cmds {
		if i == 3 {
			out = append(out, fmt.Sprintf("  and %d more", len(cmds)-3))
			break
		}
		out = append(out, wrapped("  "+l, width)...)
	}
	return out
}

func joinSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

func stateText(k readiness.Kind) string {
	n := k.Blocking()
	switch k.State {
	case readiness.Ready:
		return "ready"
	case readiness.Missing:
		return fmt.Sprintf("%d to do", n)
	case readiness.Impossible:
		return k.Why
	}
	if n > 0 {
		return fmt.Sprintf("off, %d to do", n)
	}
	return "off, ready to turn on"
}

func (s *setupScreen) paintState(k readiness.Kind, text string) string {
	switch k.State {
	case readiness.Ready:
		return s.st.ok(text)
	case readiness.Missing:
		return s.st.bad(text)
	case readiness.Off:
		return s.st.warn(text)
	}
	return s.st.dim(text)
}

// fit cuts s to width columns.
func fit(s string, width int) string {
	r := []rune(s)
	switch {
	case width <= 0:
		return ""
	case len(r) <= width:
		return s
	case width == 1:
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// wrapped breaks s at spaces to fit width, and a word longer than the line,
// which a path can be, wherever it has to.
func wrapped(s string, width int) []string {
	indent := s[:len(s)-len(strings.TrimLeft(s, " "))]
	var out []string
	line := indent
	for _, w := range strings.Fields(s) {
		switch {
		case line == indent:
			line += w
		case len([]rune(line))+1+len([]rune(w)) <= width:
			line += " " + w
		default:
			out = append(out, fit(line, width))
			line = indent + w
		}
	}
	return append(out, fit(line, width))
}
