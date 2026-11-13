package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// applyIn runs `latem setup apply` against a config directory with no
// terminal, as an agent's shell does, and keeps stdout apart from stderr:
// --json's stdout is a document.
func applyIn(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	prev := global.configDir
	t.Cleanup(func() { global.configDir = prev })
	root := newRootCommand()
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append(append([]string{"setup", "apply"}, args...), "--config-dir", dir))
	err = root.ExecuteContext(context.Background())
	return o.String(), e.String(), err
}

// fakeTool writes an executable script at dir/rel that prints out and exits
// with code: a JDK's javac or an SDK's adb, for a kind's proof.
func fakeTool(t *testing.T, dir, rel, out string, code int) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho '" + out + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // a test's script, run by the proof
		t.Fatal(err)
	}
}

// macBookAndroid is the MacBook as the ticket found it: JDK 17 and an Android
// SDK on the machine, out of a step's sight, so Android is off and two
// settings stand in the way. javac answers with code.
func macBookAndroid(t *testing.T, javacCode int) (dir string, rec *setupRecorder, jdk, sdk string) {
	t.Helper()
	dir, rec = setupMachine(t, fstest.MapFS{})
	jdk, sdk = filepath.Join(t.TempDir(), "jdk17"), filepath.Join(t.TempDir(), "sdk")
	fakeTool(t, jdk, "bin/javac", "javac 17.0.15", javacCode)
	fakeTool(t, sdk, "platform-tools/adb", "Android Debug Bridge version 1.0.41", 0)
	rec.machine.tc = system.Toolchains{
		JDKs: []int{17}, JDKHomes: []system.JDKInstall{{Major: 17, Home: jdk}},
		AndroidSDK: sdk, SDKManager: filepath.Join(sdk, "cmdline-tools", "latest", "bin", "sdkmanager"),
		AndroidPlatformTools: true, AndroidLicensed: true,
	}
	return dir, rec, jdk, sdk
}

func kindIn(t *testing.T, plan readiness.Plan, name string) readiness.Kind {
	t.Helper()
	i := slices.IndexFunc(plan.Kinds, func(k readiness.Kind) bool { return k.Name == name })
	if i < 0 {
		t.Fatalf("the plan has no %s: %+v", name, plan.Kinds)
	}
	return plan.Kinds[i]
}

func itemIDs(k readiness.Kind) []string {
	var ids []string
	for _, it := range k.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// The MacBook's Android, from off to on, the way an agent does it: the two
// settings by their ids, then the kind, which goes on once its check passes.
// --json is the plan afterwards, in the shape `setup --json` prints.
func TestSetupApply_AndroidFromTwoSettingsToOn(t *testing.T) {
	dir, rec, jdk, sdk := macBookAndroid(t, 0)
	reloads := noAgentRunning(t)

	before, err := latemIn(t, dir, "", "setup", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var plan readiness.Plan
	if err := json.Unmarshal([]byte(before), &plan); err != nil {
		t.Fatal(err)
	}
	if k := kindIn(t, plan, needs.KindAndroid); k.State != readiness.Off || !slices.Equal(itemIDs(k), []string{"java-home", "android-home"}) {
		t.Fatalf("android before = %+v", k)
	}

	stdout, stderr, err := applyIn(t, dir, "java-home", "android-home", "--json")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr)
	}
	env := loadConfigIn(t, dir).Runners[0].NativeEnv()
	if env["JAVA_HOME"] != jdk || env["ANDROID_HOME"] != sdk {
		t.Errorf("env = %v", env)
	}
	// What it says, item by item, and where the kind stands after.
	for _, want := range []string{
		"java-home: wrote JAVA_HOME = " + jdk + "; closed",
		"android-home: wrote ANDROID_HOME = " + sdk + "; closed",
		"\nNo agent is running; the next to start reads it.\n",
		"no agent service is installed here",
		"now android is off, ready to turn on",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if *reloads != 1 {
		t.Errorf("the agent was told %d times, want once for both", *reloads)
	}

	// The plan afterwards, which round-trips through the plan type to the
	// very bytes `setup --json` would print.
	plan = readiness.Plan{}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout is not the plan: %v\n%s", err, stdout)
	}
	var again bytes.Buffer
	if err := writePlan(&again, plan); err != nil {
		t.Fatal(err)
	}
	if again.String() != stdout {
		t.Errorf("--json does not round-trip:\n%s\nvs\n%s", stdout, again.String())
	}
	if now, err := latemIn(t, dir, "", "setup", "--json"); err != nil || now != stdout {
		t.Errorf("--json is not what `setup --json` says now (%v):\n%s\nvs\n%s", err, stdout, now)
	}
	if k := kindIn(t, plan, needs.KindAndroid); len(k.Items) != 0 || k.On {
		t.Errorf("android after = %+v", k)
	}

	// The kind itself: its check, then its label.
	_, stderr, err = applyIn(t, dir, "--kind", "android")
	if err != nil {
		t.Fatalf("--kind android: %v\n%s", err, stderr)
	}
	if r, _ := loadConfigIn(t, dir).Runner("macos"); !slices.Contains(r.Labels, readiness.LabelAndroid) {
		t.Errorf("labels = %q", r.Labels)
	}
	if !strings.Contains(stderr, "android: the check passed (the JDK compiles, the SDK's tools run), and the macos runner gains the label android; on") ||
		!strings.Contains(stderr, "now android is ready") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if len(rec.machine.installs) != 0 {
		t.Errorf("installed %q", rec.machine.installs)
	}

	// Again, with nothing left to do, is nothing.
	_, stderr, err = applyIn(t, dir, "--kind", "android")
	if err != nil || !strings.Contains(stderr, "android: on, on the macos runner") {
		t.Errorf("again: %v\n%s", err, stderr)
	}
}

// An id with --kind is found in that kind, and the kind is not switched on:
// an agent closing one item of two is not told the kind failed because the
// other is still in its way.
func TestSetupApply_IDsWithKindDoNotSwitchIt(t *testing.T) {
	dir, _, jdk, _ := macBookAndroid(t, 0)
	noAgentRunning(t)

	_, stderr, err := applyIn(t, dir, "java-home", "--kind", "android")
	if err != nil {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	r, _ := loadConfigIn(t, dir).Runner("macos")
	if r.NativeEnv()["JAVA_HOME"] != jdk {
		t.Errorf("env = %v", r.NativeEnv())
	}
	if slices.Contains(r.Labels, readiness.LabelAndroid) {
		t.Errorf("a label went on: %q", r.Labels)
	}
	if strings.Contains(stderr, "android: off") {
		t.Errorf("the kind was tried:\n%s", stderr)
	}
}

// A check that fails writes no label, says what the probe printed, and exits
// 1: a kind that does not work in a step's environment is sent no jobs.
func TestSetupApply_AFailingProofWritesNoLabel(t *testing.T) {
	dir, _, _, _ := macBookAndroid(t, 1)
	noAgentRunning(t)

	_, stderr, err := applyIn(t, dir, "--kind", "android")
	if exitCode(err) != ExitFailure {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	r, _ := loadConfigIn(t, dir).Runner("macos")
	if slices.Contains(r.Labels, readiness.LabelAndroid) {
		t.Errorf("a label went on: %q", r.Labels)
	}
	// The two settings were the kind's items, and closed before the check.
	if r.NativeEnv()["JAVA_HOME"] == "" {
		t.Errorf("env = %v", r.NativeEnv())
	}
	for _, want := range []string{
		"the check failed, so no label was written: the JDK compiles: exit status 1",
		"the JDK compiles printed:\n  javac 17.0.15",
		"now android is off, ready to turn on",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if !strings.Contains(err.Error(), "android: the check failed") {
		t.Errorf("err = %v", err)
	}
}

// An item's commands run as the screen runs them: through the install seam,
// as the program found, and the machine is probed again to see it closed.
// One that fails stops the rest, which may need it.
func TestSetupApply_RunsAnItemsCommands(t *testing.T) {
	dir, rec := setupMachine(t, macRepo)
	noAgentRunning(t)
	m := rec.machine
	m.dirs["xcodegen"], m.dirs["ruby"] = "/nowhere", "/nowhere"
	m.gives["brew install xcodegen"] = [2]string{"xcodegen", "/usr/local/bin"}

	_, stderr, err := applyIn(t, dir, "xcodegen")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !slices.Equal(m.installs, []string{"brew install xcodegen"}) ||
		!strings.Contains(stderr, "$ brew install xcodegen") ||
		!strings.Contains(stderr, "xcodegen: ran brew install xcodegen; closed") {
		t.Errorf("installs %q\n%s", m.installs, stderr)
	}

	// ruby's install fails in this fake; what comes after is not tried.
	m.dirs["xcodegen"] = "/nowhere"
	_, stderr, err = applyIn(t, dir, "ruby", "xcodegen")
	if exitCode(err) != ExitFailure {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	if !strings.Contains(stderr, "ruby: ran brew install ruby, which failed: exit status 1") ||
		!strings.Contains(stderr, "xcodegen: not tried, after the failure above") ||
		slices.Contains(m.installs[1:], "brew install xcodegen") {
		t.Errorf("installs %q\n%s", m.installs, stderr)
	}
}

// A program that is not installed is said, and nothing is run.
func TestSetupApply_AProgramNotInstalled(t *testing.T) {
	dir, rec := setupMachine(t, macRepo)
	noAgentRunning(t)
	rec.machine.dirs["xcodegen"] = "/nowhere"
	rec.machine.notInstalled["brew"] = true

	_, stderr, err := applyIn(t, dir, "xcodegen")
	if exitCode(err) != ExitFailure || len(rec.machine.installs) != 0 ||
		!strings.Contains(stderr, "it needs brew, which is not installed") {
		t.Errorf("exit %d, installs %q\n%s", exitCode(err), rec.machine.installs, stderr)
	}
}

// A Mac with only a container runner: the native runner every native kind
// wants is one item, written once, and only a restart gives it to the agent,
// which apply asks for and reports (A74).
func TestSetupApply_ANewRunnerRestartsTheAgent(t *testing.T) {
	rec := fakeSetup(t, true)
	dir := filepath.Join(privateTempDir(t), "box")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := renderInitConfig(initParams{Machine: "box", GOOS: "linux", GOARCH: "arm64", Refs: []string{"main"}, Docker: true})
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeReload(t, func(config.Paths) (agent.ReloadStatus, error) {
		return agent.ReloadStatus{Version: "v1", Deferred: []string{`runner "host" was added`}}, nil
	})
	asked := fakeSwitch(t, func(string) (agent.ReloadStatus, error) { return agent.ReloadStatus{Version: "v1"}, nil })

	_, stderr, err := applyIn(t, dir, "new-runner")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	r, ok := loadConfigIn(t, dir).Runner(readiness.NativeRunner)
	if !ok || r.Executor != config.ExecutorNative || !slices.Equal(r.Labels, []string{readiness.LabelNative}) {
		t.Errorf("runner = %+v", r)
	}
	for _, want := range []string{
		"new-runner: wrote a native runner named host",
		"\nThe agent restarted with it.\n",
		`its reload left these for a restart: runner "host" was added`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if len(*asked) != 1 || len(rec.machine.installs) != 0 {
		t.Errorf("switch asked %v, installs %q", *asked, rec.machine.installs)
	}
}

const actionsRepo = `on: push
jobs:
  apple:
    runs-on: macos-latest
    steps:
      - uses: actions/checkout@v4
      - uses: some/thing@v1
      - run: echo ${{ secrets.TOKEN }}
`

// An action not allowed is one more [actions] allowed entry, which says
// nothing about how it runs, so latem writes it.
func TestSetupApply_AnAllowedAction(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(actionsRepo)}})
	noAgentRunning(t)

	_, stderr, err := applyIn(t, dir, "allowed-some/thing")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if cfg := loadConfigIn(t, dir); !slices.Contains(cfg.Actions.Allowed, "some/thing") || cfg.Actions.AllowUnisolated {
		t.Errorf("actions = %+v", cfg.Actions)
	}
}

// What is a person's is refused with exit 4, and says what they would type:
// a secret's value, Xcode, and the two settings that give something up, which
// off a terminal are not written however they are asked for. What latem can
// do is still done.
func TestSetupApply_APersonsItemsOffATerminal(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(actionsRepo)}})
	noAgentRunning(t)
	before := readConfigFile(t, dir)

	_, stderr, err := applyIn(t, dir, "secrets-app", "Xcode", "unisolated", "moving-refs")
	if exitCode(err) != ExitNoTerminal {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	if readConfigFile(t, dir) != before || len(rec.machine.installs) != 0 {
		t.Errorf("it changed something; installs %q", rec.machine.installs)
	}
	for _, want := range []string{
		"secrets-app: a person's to type", "  latem secrets set --repo app TOKEN",
		"Xcode: a person's to do:\n  install Xcode from the App Store, or with xcodes",
		"unisolated: it gives something up, so only a person at a terminal may write it, and neither stdin nor stdout is one.",
		"  it writes [actions] allow_unisolated = true",
		"  latem setup apply unisolated --config-dir " + shellWord(dir),
		"  latem setup apply moving-refs --config-dir " + shellWord(dir),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if !strings.Contains(err.Error(), "secrets-app, Xcode, unisolated, moving-refs are for the person who runs this machine") {
		t.Errorf("err = %v", err)
	}

	// --kind with them in it: latem's items are done and the person's are
	// left, with the kind, which was already on.
	_, stderr, err = applyIn(t, dir, "--kind", "macos")
	if exitCode(err) != ExitNoTerminal {
		t.Fatalf("--kind: exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	if !slices.Contains(loadConfigIn(t, dir).Actions.Allowed, "some/thing") ||
		!strings.Contains(stderr, "macos: on, on the macos runner") ||
		!strings.Contains(stderr, "now macos is 3 to do") {
		t.Errorf("--kind:\n%s", stderr)
	}
}

// At a terminal the two settings are asked about, as the screen asks, and
// the answer defaults to no. A command that is yours stays yours there too:
// latem does not type it.
func TestSetupApply_AtATerminal(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(actionsRepo)}})
	noAgentRunning(t)
	prev := global.configDir
	t.Cleanup(func() { global.configDir = prev })
	const asked = "Write this to config.toml? [y/N]"

	screen, stderr, err := atTerminal(t, terminalSession{prompt: asked, answer: "y"},
		"setup", "apply", "unisolated", "--config-dir", dir)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, screen, stderr)
	}
	if !loadConfigIn(t, dir).Actions.AllowUnisolated || !strings.Contains(screen, "They run as you") ||
		!strings.Contains(stderr, "unisolated: wrote [actions] allow_unisolated = true; closed") {
		t.Errorf("screen:\n%s\nstderr:\n%s", screen, stderr)
	}

	before := readConfigFile(t, dir)
	_, stderr, err = atTerminal(t, terminalSession{prompt: asked, answer: ""},
		"setup", "apply", "moving-refs", "--config-dir", dir)
	if exitCode(err) != ExitFailure || readConfigFile(t, dir) != before ||
		!strings.Contains(stderr, "moving-refs: left as it is: the answer was no") {
		t.Errorf("no: exit %d (%v)\n%s", exitCode(err), err, stderr)
	}

	_, stderr, err = atTerminal(t, terminalSession{}, "setup", "apply", "secrets-app", "--config-dir", dir)
	if exitCode(err) != ExitNoTerminal || !strings.Contains(stderr, "latem secrets set --repo app TOKEN") {
		t.Errorf("yours: exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
}

// An id or a kind the plan does not have is a usage error that lists what it
// does have, before anything is done.
func TestSetupApply_UsageErrors(t *testing.T) {
	dir, rec, _, _ := macBookAndroid(t, 0)
	before := readConfigFile(t, dir)

	cases := []struct {
		args []string
		want string
	}{
		{nil, "name an item by its id, or a kind with --kind"},
		{[]string{"java-home", "jdk-home"}, "jdk-home is not an item here. The ids there are, by kind: ios: Xcode; android: java-home, android-home"},
		{[]string{"java-home", "--kind", "ios"}, "java-home is not an item here. The ids there are, by kind: ios: Xcode"},
		{[]string{"--kind", "windows"}, "--kind windows: this machine's plan has no such kind. It has ios, android, linux"},
	}
	for _, c := range cases {
		_, stderr, err := applyIn(t, dir, c.args...)
		if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: exit %d, %v\n%s", c.args, exitCode(err), err, stderr)
		}
	}
	if readConfigFile(t, dir) != before || len(rec.machine.installs) != 0 {
		t.Error("a usage error changed something")
	}

	_, _, err := applyIn(t, filepath.Join(privateTempDir(t), "none"), "java-home")
	if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "no config yet") {
		t.Errorf("no config: %v", err)
	}
}
