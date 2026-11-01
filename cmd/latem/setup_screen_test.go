package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// screenWorld is what a screen's hooks would have done, and the plan it would
// see next.
type screenWorld struct {
	plans   []readiness.Plan // each refresh takes the next, and the last stays
	ran     []string
	applied []readiness.Change
	shells  [][]string
	proved  []string
	proof   error
	applyEr error
}

func (w *screenWorld) hooks() screenHooks {
	return screenHooks{
		refresh: func() (readiness.Plan, error) {
			if len(w.plans) > 1 {
				w.plans = w.plans[1:]
			}
			return w.plans[0], nil
		},
		run: func(_ readiness.Kind, it readiness.Item) error {
			w.ran = append(w.ran, commandLines(it.Run)...)
			return nil
		},
		apply: func(c []readiness.Change) error {
			if w.applyEr != nil {
				return w.applyEr
			}
			w.applied = append(w.applied, c...)
			return nil
		},
		shell: func(_ string, lines []string) error {
			w.shells = append(w.shells, lines)
			return nil
		},
		prove: func(k readiness.Kind) error {
			w.proved = append(w.proved, k.Name)
			return w.proof
		},
	}
}

func desktopPlan() readiness.Plan {
	return readiness.Plan{Machine: "desktop", Host: system.Host{OS: "Linux", Arch: "amd64", Name: "Arch Linux"}, Kinds: []readiness.Kind{
		{Name: needs.KindAndroid, Title: "Android", State: readiness.Off, Runner: "host", Label: readiness.LabelAndroid, Repos: []string{"tsedal"},
			Notes: []string{"tsedal preview.yml android asks for macos, xcode; it comes here once its workflow says runs-on: [android]"},
			Items: []readiness.Item{
				{ID: "JDK 17", Title: "JDK 17", Have: "JDK [27]", Yours: [][]string{{"sudo", "pacman", "-S", "--needed", "jdk17-openjdk"}}},
				{ID: "tools", Title: "the Android command-line tools", Run: [][]string{{"latem", "setup", "android-sdk"}}},
				{ID: "unisolated", Title: "remote actions allowed on this machine", Warning: "They run as you.",
					Config: []readiness.Change{{What: readiness.ChangeUnisolated, Line: "[actions] allow_unisolated = true"}}},
				{ID: "manual", Title: "something only you can do", Manual: []string{"do it by hand"}},
				{ID: "soft", Title: "ANDROID_HOME set", Soft: true},
			}},
		{Name: needs.KindLinux, Title: "Linux", State: readiness.Ready, On: true, Runner: "linux", Repos: []string{"latem", "tsedal"}},
		{Name: needs.KindIOS, Title: "iOS", State: readiness.Impossible, Why: "needs a Mac"},
	}}
}

// readyAndroid is the plan once nothing is in the way.
func readyAndroid() readiness.Plan {
	p := desktopPlan()
	p.Kinds[0].Items = nil
	return p
}

func press(s *setupScreen, keys ...keyPress) {
	for _, k := range keys {
		s.key(k)
	}
}

var (
	down  = keyPress{code: keyDown}
	enter = keyPress{code: keyEnter}
	left  = keyPress{code: keyLeft}
	space = keyPress{r: ' '}
	yes   = keyPress{r: 'y'}
	no    = keyPress{r: 'n'}
)

func text(s *setupScreen, w, h int) string { return strings.Join(s.view(w, h), "\n") }

func TestScreen_ShowsWhatTheMachineBuilds(t *testing.T) {
	s := newSetupScreen(desktopPlan(), (&screenWorld{plans: []readiness.Plan{desktopPlan()}}).hooks(), styler{})
	got := text(s, 130, 30)
	for _, want := range []string{
		"latem setup · desktop · Arch Linux amd64",
		"> [ ] Android", "off, 4 to do", "tsedal",
		"[x] Linux", "ready",
		"[ ] iOS", "needs a Mac",
		"Android, on the host runner",
		"✗ JDK 17", "JDK [27]", "~ ANDROID_HOME set",
		"it comes here once its workflow says runs-on: [android]",
		"q done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("screen lacks %q:\n%s", want, got)
		}
	}
}

func TestScreen_FitsWhatItIsGiven(t *testing.T) {
	s := newSetupScreen(desktopPlan(), (&screenWorld{plans: []readiness.Plan{desktopPlan()}}).hooks(), styler{})
	press(s, enter, down, down, down)
	for _, size := range [][2]int{{120, 40}, {80, 24}, {60, 12}, {40, 8}, {20, 6}} {
		lines := s.view(size[0], size[1])
		if len(lines) != size[1] {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		for _, l := range lines {
			if n := len([]rune(l)); n > size[0] {
				t.Errorf("%v: %d columns in %q", size, n, l)
			}
		}
		if !strings.Contains(lines[len(lines)-1], "move") {
			t.Errorf("%v: the keys scrolled away: %q", size, lines)
		}
	}
	// The thing under the cursor stays on a short screen.
	if got := text(s, 80, 12); !strings.Contains(got, "> ✗ something only you can do") {
		t.Errorf("the cursor scrolled away:\n%s", got)
	}
}

// A command latem runs asks first, and a no leaves it unrun.
func TestScreen_ACommandLatemRunsAsksFirst(t *testing.T) {
	w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, enter, down, enter)
	if got := text(s, 100, 30); !strings.Contains(got, "Run this?") || !strings.Contains(got, "latem setup android-sdk") {
		t.Fatalf("no question:\n%s", got)
	}
	press(s, no)
	if len(w.ran) != 0 || !strings.Contains(s.status, "Left as it is") {
		t.Errorf("ran %q after a no; status %q", w.ran, s.status)
	}
	press(s, enter, yes)
	if !slices.Equal(w.ran, []string{"latem setup android-sdk"}) {
		t.Errorf("ran %q", w.ran)
	}
}

// What wants sudo goes to a shell: latem types nothing, and nothing runs
// without the person.
func TestScreen_WhatIsYoursOpensAShell(t *testing.T) {
	w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, enter, enter)
	if len(w.ran) != 0 || len(w.shells) != 1 || w.shells[0][0] != "sudo pacman -S --needed jdk17-openjdk" {
		t.Errorf("ran %q, shells %q", w.ran, w.shells)
	}
}

// Giving something up says what, before asking.
func TestScreen_AChangeSaysWhatItGivesUp(t *testing.T) {
	w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, enter, down, down, enter)
	got := text(s, 100, 30)
	if !strings.Contains(got, "They run as you.") || !strings.Contains(got, "allow_unisolated = true") {
		t.Fatalf("no warning:\n%s", got)
	}
	press(s, yes)
	if len(w.applied) != 1 || w.applied[0].What != readiness.ChangeUnisolated {
		t.Errorf("applied %+v", w.applied)
	}

	w.applyEr = errors.New("would not load")
	press(s, enter, yes)
	if !strings.Contains(s.status, "left as it was: would not load") {
		t.Errorf("status = %q", s.status)
	}
}

func TestScreen_ManualItemsAreSaid(t *testing.T) {
	w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, enter, down, down, down)
	if got := text(s, 100, 30); !strings.Contains(got, "do it by hand") {
		t.Fatalf("not said:\n%s", got)
	}
	press(s, enter)
	if len(w.ran)+len(w.applied)+len(w.shells) != 0 {
		t.Errorf("did something: %+v", w)
	}
}

// A kind goes on only with nothing in the way, and only past a proof and a
// yes: the label is what sends it real jobs.
func TestScreen_SwitchingAKindOn(t *testing.T) {
	w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, space)
	if len(w.proved) != 0 || !strings.Contains(s.status, "4 to do before this machine can take Android jobs") {
		t.Errorf("proved %q; status %q", w.proved, s.status)
	}

	w = &screenWorld{plans: []readiness.Plan{readyAndroid()}, proof: errors.New("adb: not found")}
	s = newSetupScreen(readyAndroid(), w.hooks(), styler{})
	press(s, space)
	if s.ask != nil || len(w.applied) != 0 || !strings.Contains(s.status, "adb: not found") {
		t.Errorf("a failed proof: ask %+v, applied %+v, status %q", s.ask, w.applied, s.status)
	}

	w.proof = nil
	press(s, space)
	if s.ask == nil || len(w.applied) != 0 {
		t.Fatalf("a label went on with no yes: %+v", w.applied)
	}
	if got := text(s, 120, 30); !strings.Contains(got, "Add the label android to the host runner?") ||
		!strings.Contains(got, "runs-on: [android]") {
		t.Errorf("question:\n%s", got)
	}
	press(s, yes)
	if len(w.applied) != 1 || w.applied[0].What != readiness.ChangeLabelAdd || w.applied[0].Value != "android" || w.applied[0].Runner != "host" {
		t.Errorf("applied %+v", w.applied)
	}
}

func TestScreen_SwitchingAKindOff(t *testing.T) {
	p := readyAndroid()
	p.Kinds[0].On, p.Kinds[0].Labelled, p.Kinds[0].State = true, true, readiness.Ready
	w := &screenWorld{plans: []readiness.Plan{p}}
	s := newSetupScreen(p, w.hooks(), styler{})
	press(s, space, yes)
	if len(w.applied) != 1 || w.applied[0].What != readiness.ChangeLabelDrop {
		t.Errorf("applied %+v", w.applied)
	}
}

func TestScreen_KindsWithNoSwitchSayWhy(t *testing.T) {
	w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, down, space)
	if !strings.Contains(s.status, "linux runner's labels") {
		t.Errorf("status = %q", s.status)
	}
	press(s, down, space)
	if !strings.Contains(s.status, "iOS needs a Mac") {
		t.Errorf("status = %q", s.status)
	}
}

// After a thing is done, the cursor stays where the person was working,
// rather than jumping back to the top of the list.
func TestScreen_KeepsItsPlaceWhenAThingIsDone(t *testing.T) {
	after := desktopPlan()
	after.Kinds[0].Items = after.Kinds[0].Items[1:] // the JDK is in
	w := &screenWorld{plans: []readiness.Plan{desktopPlan(), after}}
	s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
	press(s, enter, down, down)
	press(s, keyPress{r: 'r'})
	if !s.inItems || after.Kinds[0].Items[s.item].ID != "unisolated" {
		t.Errorf("cursor at %d, in items %v", s.item, s.inItems)
	}
	press(s, left)
	if s.inItems {
		t.Error("left did not go back")
	}
}

func TestScreen_QuitFromAnywhere(t *testing.T) {
	for _, keys := range [][]keyPress{
		{{r: 'q'}},
		{enter, {r: 'q'}},
		{{code: keyInterrupt}},
		{enter, down, enter, {code: keyInterrupt}}, // from inside a question
	} {
		w := &screenWorld{plans: []readiness.Plan{desktopPlan()}}
		s := newSetupScreen(desktopPlan(), w.hooks(), styler{})
		press(s, keys...)
		if !s.done {
			t.Errorf("%v did not quit", keys)
		}
	}
}

func TestParseKeys(t *testing.T) {
	got := parseKeys([]byte("\x1b[A\x1bOBj \r\x03\x1b[Z\x1b"))
	want := []keyPress{{code: keyUp}, {code: keyDown}, {r: 'j'}, {r: ' '}, {code: keyEnter}, {code: keyInterrupt}, {code: keyEsc}}
	if !slices.Equal(got, want) {
		t.Errorf("keys = %+v", got)
	}
}

func TestWrapped(t *testing.T) {
	got := wrapped("  one two three four", 10)
	if !slices.Equal(got, []string{"  one two", "  three", "  four"}) {
		t.Errorf("wrapped = %q", got)
	}
	if got := wrapped(strings.Repeat("x", 30), 10); len(got) != 1 || len([]rune(got[0])) != 10 {
		t.Errorf("a long word = %q", got)
	}
}

// The plan a log gets, off a terminal.
func TestPrintPlan(t *testing.T) {
	var b strings.Builder
	p := desktopPlan()
	p.Unread = []string{"app: no mirror"}
	p.Unclassified = []string{"a b.yml c"}
	printPlan(&b, p)
	for _, want := range []string{
		"Android            off, 4 to do: JDK 17, the Android command-line tools, remote actions allowed on this machine, and 1 more",
		"Linux              ready", "iOS                needs a Mac", "runs-on: [android]",
		"not read: app: no mirror", "1 job not classified: a b.yml c",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, b.String())
		}
	}
}

func TestProveKind(t *testing.T) {
	ok := readiness.Kind{Proof: []readiness.Probe{{Title: "works", Argv: []string{"/bin/sh", "-c", "test \"$JAVA_HOME\" = /jvm && test -z \"$NOT_PASSED\""},
		Env: map[string]string{"JAVA_HOME": "/jvm"}}}}
	t.Setenv("NOT_PASSED", "from the shell")
	if err := proveKind(context.Background(), "/usr/bin:/bin", ok); err != nil {
		t.Errorf("a step's environment: %v", err)
	}
	bad := readiness.Kind{Proof: []readiness.Probe{{Title: "fails", Argv: []string{"/bin/sh", "-c", "echo no such tool; exit 3"}}}}
	err := proveKind(context.Background(), "/usr/bin:/bin", bad)
	if err == nil || !strings.Contains(err.Error(), "fails") || !strings.Contains(err.Error(), "no such tool") {
		t.Errorf("err = %v", err)
	}
}
