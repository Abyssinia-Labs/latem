package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // the manifest's own checksum
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A Linux machine as `latem init` writes it, with a repository beside it.
func linuxMachine(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(privateTempDir(t), "box")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := renderInitConfig(initParams{Machine: "box", GOOS: "linux", GOARCH: "amd64", Refs: []string{"main"}, Docker: true})
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func useConfigDir(t *testing.T, dir string) {
	t.Helper()
	prev := global.configDir
	global.configDir = dir
	t.Cleanup(func() { global.configDir = prev })
}

// The order a Linux machine goes through them: a runner, then what it is told,
// then the label that sends it jobs. Each edit reads what the last one wrote,
// and what comes out loads.
func TestApplyChanges_ANativeRunnerFromNothing(t *testing.T) {
	dir := linuxMachine(t)
	useConfigDir(t, dir)
	for _, c := range [][]readiness.Change{
		{{What: readiness.ChangeNewRunner, Value: readiness.NativeRunner}},
		{{What: readiness.ChangeEnv, Runner: "host", Key: "JAVA_HOME", Value: "/usr/lib/jvm/java-17-openjdk"},
			{What: readiness.ChangeEnv, Runner: "host", Key: "ANDROID_HOME", Value: "/home/me/Android/Sdk"}},
		{{What: readiness.ChangeLabelAdd, Runner: "host", Value: "android"}},
		{{What: readiness.ChangeUnisolated}, {What: readiness.ChangeMovingRefs}},
		{{What: readiness.ChangeAllowed, Value: "oven-sh/setup-bun"}},
	} {
		if err := applyChanges(c); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	cfg := loadConfigIn(t, dir)
	r, ok := cfg.Runner("host")
	if !ok || r.Executor != config.ExecutorNative || !slices.Equal(r.Labels, []string{"native", "android"}) || r.Concurrency != 1 {
		t.Fatalf("runner = %+v", r)
	}
	if env := r.NativeEnv(); env["JAVA_HOME"] != "/usr/lib/jvm/java-17-openjdk" || env["ANDROID_HOME"] != "/home/me/Android/Sdk" {
		t.Errorf("env = %v", env)
	}
	if !cfg.Actions.AllowUnisolated || cfg.Actions.PinningRequired() || !slices.Contains(cfg.Actions.Allowed, "oven-sh/setup-bun") {
		t.Errorf("actions = %+v", cfg.Actions)
	}
	if linux, ok := cfg.Runner("linux"); !ok || !slices.Contains(linux.Labels, "linux") {
		t.Errorf("the docker runner changed: %+v", linux)
	}

	if err := applyChanges([]readiness.Change{{What: readiness.ChangeLabelDrop, Runner: "host", Value: "ANDROID"}}); err != nil {
		t.Fatal(err)
	}
	r, _ = loadConfigIn(t, dir).Runner("host")
	if !slices.Equal(r.Labels, []string{"native"}) {
		t.Errorf("labels after a drop = %q", r.Labels)
	}
}

// A runner's last label cannot go: a runner with none does not load.
func TestApplyChanges_ARunnersLastLabel(t *testing.T) {
	dir := linuxMachine(t)
	useConfigDir(t, dir)
	if err := applyChanges([]readiness.Change{{What: readiness.ChangeNewRunner, Value: "host"}}); err != nil {
		t.Fatal(err)
	}
	if err := applyChanges([]readiness.Change{{What: readiness.ChangeLabelDrop, Runner: "host", Value: "native"}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := loadConfigIn(t, dir).Runner("host"); !slices.Equal(r.Labels, []string{"native"}) {
		t.Errorf("labels = %q", r.Labels)
	}
}

func TestApplyChanges_RefusesARunnerItDoesNotHave(t *testing.T) {
	dir := linuxMachine(t)
	useConfigDir(t, dir)
	before, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	err := applyChanges([]readiness.Change{{What: readiness.ChangeLabelAdd, Runner: "ghost", Value: "android"}})
	after, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err == nil || !bytes.Equal(before, after) {
		t.Errorf("err = %v; config changed: %v", err, !bytes.Equal(before, after))
	}
}

// --json is the plan and nothing else, and it changes nothing: a script reads
// it, and a person may run it to see.
func TestSetup_JSONIsThePlanAndTouchesNothing(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(`on: push
jobs:
  apple:
    runs-on: macos-latest
    steps:
      - run: xcodegen generate
`)}})
	rec.machine.dirs["xcodegen"] = "/nowhere"
	before, _ := os.ReadFile(filepath.Join(dir, "config.toml"))

	out, err := latemIn(t, dir, "", "setup", "--json")
	if err != nil {
		t.Fatalf("setup --json: %v\n%s", err, out)
	}
	var plan readiness.Plan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	i := slices.IndexFunc(plan.Kinds, func(k readiness.Kind) bool { return k.Name == needs.KindXcode })
	if i < 0 {
		t.Fatalf("kinds = %+v", plan.Kinds)
	}
	if k := plan.Kinds[i]; k.State != readiness.Missing || k.Runner != "macos" || len(k.Items) == 0 ||
		k.Items[0].Title != "XcodeGen" || commandLines(k.Items[0].Run)[0] != "brew install xcodegen" {
		t.Errorf("kind = %+v", k)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !bytes.Equal(before, after) || len(rec.machine.installs) != 0 || rec.svc.installs+rec.svc.restart != 0 {
		t.Errorf("--json touched something: installs %q", rec.machine.installs)
	}
}

func TestSetup_JSONWithNoConfig(t *testing.T) {
	fakeSetup(t, true)
	if out, err := latemIn(t, filepath.Join(privateTempDir(t), "none"), "", "setup", "--json"); err == nil {
		t.Errorf("no config, and it said: %s", out)
	}
}

// Off a terminal the toolchains step ends with the plan, jobs for other
// machines included, and the run goes on.
func TestSetup_PrintsThePlanOffATerminal(t *testing.T) {
	dir, _ := setupMachine(t, macRepo)
	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"what this machine builds", "Linux", "6/6 doctor"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func toolsZip(t *testing.T, files map[string]string, modes map[string]fs.FileMode) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		mode := fs.FileMode(0o644)
		if m, ok := modes[name]; ok {
			mode = m
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// serveSDK serves a manifest naming archive, with sum as its checksum, or the
// archive's own when sum is empty.
func serveSDK(t *testing.T, archive []byte, sum string) {
	t.Helper()
	if sum == "" {
		h := sha1.Sum(archive) //nolint:gosec // the manifest's own checksum
		sum = hex.EncodeToString(h[:])
	}
	manifest := fmt.Sprintf(`<?xml version="1.0"?><sdk:sdk-repository xmlns:sdk="http://schemas.android.com/sdk/android/repo/repository2/03">
<remotePackage path="cmdline-tools;11.0"><archives><archive><complete><size>1</size><checksum>0</checksum><url>old.zip</url></complete><host-os>linux</host-os></archive></archives></remotePackage>
<remotePackage path="cmdline-tools;latest"><archives>
<archive><complete><size>9</size><checksum>0</checksum><url>mac.zip</url></complete><host-os>macosx</host-os></archive>
<archive><complete><size>%d</size><checksum type="sha1">%s</checksum><url>tools.zip</url></complete><host-os>linux</host-os></archive>
</archives></remotePackage></sdk:sdk-repository>`, len(archive), sum)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repository2-3.xml":
			_, _ = w.Write([]byte(manifest))
		case "/tools.zip":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prev := androidRepository
	androidRepository = srv.URL + "/"
	t.Cleanup(func() { androidRepository = prev })
}

// The agent's look at its own machine reads the mirrors it already has. A
// clone from there would run on every toolchain beat, with the service's
// credentials, for a page.
func TestMachineReadiness_ReadsOnlyTheMirror(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(`on: push
jobs:
  apple:
    runs-on: macos-latest
    steps:
      - run: xcodegen generate
`)}})
	rec.machine.dirs["xcodegen"] = "/nowhere"
	cfg := loadConfigIn(t, dir)

	var from []treeSource
	prev := readRepo
	readRepo = func(ctx context.Context, cfg *config.Config, key string, f treeSource) (*repoFiles, error) {
		from = append(from, f)
		return prev(ctx, cfg, key, f)
	}
	t.Cleanup(func() { readRepo = prev })

	got, err := machineReadiness(t.Context(), cfg, rec.machine.deps())
	if err != nil {
		t.Fatal(err)
	}
	if len(from) == 0 || slices.ContainsFunc(from, func(f treeSource) bool { return f != fromMirror }) {
		t.Errorf("read from %v", from)
	}
	i := slices.IndexFunc(got.Kinds, func(k protocol.ReadinessKind) bool { return k.Kind == needs.KindXcode })
	if i < 0 || got.Kinds[i].State != string(readiness.Missing) || got.Kinds[i].Items[0].Title != "XcodeGen" {
		t.Errorf("kinds = %+v", got.Kinds)
	}
}

// The desktop's card said it had no Docker daemon, over a job that its own
// LATEM variable keeps off it every time. A variable decides that for good; an
// event a check never tries does not, so a job stopped only by that still counts.
func TestMachineReadiness_AJobTheMachinesVariablesStop(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(`on: push
jobs:
  github-only:
    if: vars.LATEM != 'true'
    runs-on: macos-latest
    steps:
      - run: xcodegen generate
  on-a-release:
    if: github.event_name == 'release'
    runs-on: macos-latest
    steps:
      - run: xcodegen generate
`)}})
	rec.machine.dirs["xcodegen"] = "/nowhere"
	jobs := func() int {
		got, err := machineReadiness(t.Context(), loadConfigIn(t, dir), rec.machine.deps())
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(got.Kinds, func(k protocol.ReadinessKind) bool { return k.Kind == needs.KindXcode })
		if i < 0 {
			t.Fatalf("kinds = %+v", got.Kinds)
		}
		return got.Kinds[i].Jobs
	}
	if n := jobs(); n != 2 {
		t.Errorf("with LATEM unset, %d jobs: its if: is true", n)
	}

	path := filepath.Join(dir, "config.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, "\n[repos.app.vars]\nLATEM = \"true\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := jobs(); n != 1 {
		t.Errorf("with LATEM set, %d jobs: the variable stops one and not the other", n)
	}
}

// A repository with no mirror yet is one the machine could not read. The
// others still count, and a page says so.
func TestMachineReadiness_ARepositoryWithNoMirror(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{})
	prev := readRepo
	readRepo = func(context.Context, *config.Config, string, treeSource) (*repoFiles, error) {
		return nil, errors.New("the agent has no mirror yet")
	}
	t.Cleanup(func() { readRepo = prev })

	got, err := machineReadiness(t.Context(), loadConfigIn(t, dir), rec.machine.deps())
	if err != nil {
		t.Fatal(err)
	}
	if got.Unread != 1 {
		t.Errorf("unread = %d", got.Unread)
	}
}

// The heartbeat's explanation runs in the agent, on repositories' files, on a
// timer. A panic in it must not stop the machine's builds.
func TestMachineReadiness_APanicIsAnError(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{})
	prev := readRepo
	readRepo = func(context.Context, *config.Config, string, treeSource) (*repoFiles, error) {
		panic("a workflow nobody expected")
	}
	t.Cleanup(func() { readRepo = prev })

	got, err := machineReadiness(t.Context(), loadConfigIn(t, dir), rec.machine.deps())
	if err == nil || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestInstallCmdlineTools(t *testing.T) {
	serveSDK(t, toolsZip(t, map[string]string{
		"cmdline-tools/bin/sdkmanager": "#!/bin/sh\n",
		"cmdline-tools/lib/tools.jar":  "jar",
		"unrelated/file":               "x",
	}, map[string]fs.FileMode{"cmdline-tools/bin/sdkmanager": 0o755}), "")
	dest := t.TempDir()
	var out strings.Builder
	if err := installCmdlineTools(t.Context(), &out, http.DefaultClient, dest); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	bin := filepath.Join(dest, "cmdline-tools", "latest", "bin", "sdkmanager")
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("sdkmanager: %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "cmdline-tools", "latest", "unrelated")); err == nil {
		t.Error("unpacked what is outside cmdline-tools/")
	}
	left, _ := os.ReadDir(filepath.Join(dest, "cmdline-tools"))
	if len(left) != 1 {
		t.Errorf("left behind: %v", left)
	}

	// Twice is safe, and asks Google for nothing.
	androidRepository = "http://127.0.0.1:1/"
	out.Reset()
	if err := installCmdlineTools(t.Context(), &out, http.DefaultClient, dest); err != nil || !strings.Contains(out.String(), "already") {
		t.Errorf("again: %v, %s", err, out.String())
	}
}

// Nothing lands in the SDK from an archive that is not the one named, or one
// that reaches outside itself, and what was there before is untouched.
func TestInstallCmdlineTools_RefusesABadArchive(t *testing.T) {
	good := toolsZip(t, map[string]string{"cmdline-tools/bin/sdkmanager": "x"}, nil)
	for name, c := range map[string]struct {
		archive []byte
		sum     string
		want    string
	}{
		"wrong checksum": {good, strings.Repeat("0", 40), "does not match"},
		"zip slip":       {toolsZip(t, map[string]string{"cmdline-tools/../../../evil": "x"}, nil), "", "outside itself"},
		"no sdkmanager":  {toolsZip(t, map[string]string{"cmdline-tools/README": "x"}, nil), "", "no bin/sdkmanager"},
		"a link": {toolsZip(t, map[string]string{"cmdline-tools/bin/sdkmanager": "/etc/passwd"},
			map[string]fs.FileMode{"cmdline-tools/bin/sdkmanager": fs.ModeSymlink | 0o777}), "", "not a file"},
	} {
		t.Run(name, func(t *testing.T) {
			serveSDK(t, c.archive, c.sum)
			dest := t.TempDir()
			var out strings.Builder
			err := installCmdlineTools(t.Context(), &out, http.DefaultClient, dest)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v", err)
			}
			if entries, _ := os.ReadDir(filepath.Join(dest, "cmdline-tools")); len(entries) != 0 {
				t.Errorf("left behind: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); err == nil {
				t.Error("wrote outside the destination")
			}
		})
	}
}

// fakeSwitch replaces the SIGUSR2 path for one test and records what it was
// asked to run.
func fakeSwitch(t *testing.T, answer func(want string) (agent.ReloadStatus, error)) *[]string {
	t.Helper()
	var asked []string
	prev := signalSwitch
	signalSwitch = func(_ config.Paths, want string, _ time.Duration) (agent.ReloadStatus, error) {
		asked = append(asked, want)
		return answer(want)
	}
	t.Cleanup(func() { signalSwitch = prev })
	return &asked
}

// What setup writes reaches the agent while setup's screen is still up: a
// reload for what one applies, and a restart that waits for runs for a
// runner's labels, which only a restart reads (A74).
func TestTakeConfig(t *testing.T) {
	paths := config.DerivePaths(t.TempDir())
	deferred := []string{`runner "host" was changed`}
	cases := []struct {
		name   string
		reload func(config.Paths) (agent.ReloadStatus, error)
		sw     func(string) (agent.ReloadStatus, error)
		asked  int
		want   string
	}{
		{"no agent", func(config.Paths) (agent.ReloadStatus, error) { return agent.ReloadStatus{}, agent.ErrNoAgent },
			nil, 0, "No agent is running"},
		{"reloaded", func(config.Paths) (agent.ReloadStatus, error) { return agent.ReloadStatus{Version: "v1"}, nil },
			nil, 0, "The agent has it."},
		{"refused", func(config.Paths) (agent.ReloadStatus, error) {
			return agent.ReloadStatus{Version: "v1", Error: "line 3: bad"}, nil
		}, nil, 0, "kept the config it had: line 3: bad"},
		{"restarted", func(config.Paths) (agent.ReloadStatus, error) {
			return agent.ReloadStatus{Version: "v1", Deferred: deferred}, nil
		}, func(string) (agent.ReloadStatus, error) { return agent.ReloadStatus{Version: "v1"}, nil }, 1, "restarted with it"},
		{"busy", func(config.Paths) (agent.ReloadStatus, error) {
			return agent.ReloadStatus{Version: "v1", Deferred: deferred}, nil
		}, func(string) (agent.ReloadStatus, error) {
			return agent.ReloadStatus{Switch: &agent.SwitchState{State: agent.SwitchWaiting, Runs: 2}}, nil
		}, 1, "once 2 runs finish"},
		{"too old to restart itself", func(config.Paths) (agent.ReloadStatus, error) {
			return agent.ReloadStatus{Version: "v1", Deferred: deferred}, nil
		}, func(string) (agent.ReloadStatus, error) { return agent.ReloadStatus{}, agent.ErrCannotSwitch }, 1, "latem service restart"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeReload(t, tc.reload)
			asked := fakeSwitch(t, func(want string) (agent.ReloadStatus, error) {
				if tc.sw == nil {
					t.Fatal("asked the agent to restart with nothing deferred")
				}
				return tc.sw(want)
			})
			got := takeConfig(paths)
			if !strings.Contains(got, tc.want) {
				t.Errorf("takeConfig = %q, want it to say %q", got, tc.want)
			}
			if len(*asked) != tc.asked || (tc.asked > 0 && (*asked)[0] != "v1") {
				t.Errorf("asked to switch %v, want %d time(s) to the version it runs", *asked, tc.asked)
			}
		})
	}
}
