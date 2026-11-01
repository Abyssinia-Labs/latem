package needs

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
)

// Each fixture here reproduces a pattern from a real fleet's workflows — the
// shape of it, not the file: those repositories are private, and this one is
// not.

func jobOf(t *testing.T, fsys fstest.MapFS, file, key string, leg Leg) Report {
	t.Helper()
	wf, err := workflow.Parse(fsys[file].Data, file)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return Job(fsys, wf, key, leg)
}

func requireKeys(t *testing.T, r Report, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if !r.Has(k) {
			t.Errorf("%s not required; report = %+v", k, r)
		}
	}
}

func refuseKeys(t *testing.T, r Report, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if q, ok := r.Get(k); ok {
			t.Errorf("%s required, from %+v", k, q.Evidence)
		}
	}
}

var mac = Leg{Labels: []string{"macos"}}
var linux = Leg{Labels: []string{"linux"}}

// An Apple job that selects Xcode 27 by a script, runs a simulator through
// another, and has Bun, Playwright and Swift packages beside it.
func TestJob_AnIOSJobWithEverythingAroundIt(t *testing.T) {
	fsys := fstest.MapFS{
		".github/workflows/checks.yml": {Data: []byte(`
on: pull_request
jobs:
  apple:
    runs-on: macos-26
    steps:
      - uses: actions/checkout@v5
      - uses: oven-sh/setup-bun@v2
        with: {bun-version: 1.3.3}
      - run: bun install && bun run fixtures
      - run: |
          brew install xcodegen
          sudo xcode-select -s "$(scripts/find-xcode.sh 27)"
          xcodegen generate
          xcrun swift-format lint -r Sources
          swift test --package-path Kit
      - run: |
          SIM="$(scripts/ci-simulator.sh)"
          xcodebuild -scheme App -destination "id=$SIM" test
`)},
		"scripts/find-xcode.sh":   {Data: []byte("for app in /Applications/Xcode*.app; do :; done\n")},
		"scripts/ci-simulator.sh": {Data: []byte("xcrun simctl list devices available --json | python3 -c 'import json'\n")},
		"package.json":            {Data: []byte(`{"scripts":{"fixtures":"bun scripts/check-fixtures.ts"}}`)},
	}
	r := jobOf(t, fsys, ".github/workflows/checks.yml", "apple", mac)
	if r.Kind != KindIOS || r.Platform != "macOS" {
		t.Errorf("kind %s on %s, want ios on macOS", r.Kind, r.Platform)
	}
	requireKeys(t, r, Xcode, IOSSDK, IOSSimulator, XcodeGen, Swift, Bun, Brew, Python, Sudo)
	if q, _ := r.Get(Xcode); q.Version != "27" || q.VersionStrength != Inferred || q.Strength != Invoked {
		t.Errorf("xcode = %+v, want it invoked, at 27 inferred from find-xcode.sh 27", q)
	}
	if q, _ := r.Get(Bun); q.ProvidedBy != "oven-sh/setup-bun" || q.Version != "1.3.3" {
		t.Errorf("bun = %+v, want provided by setup-bun at 1.3.3", q)
	}
	if q, _ := r.Get(IOSSimulator); q.Evidence.File != "scripts/ci-simulator.sh" || q.Evidence.Line != 1 {
		t.Errorf("simulator evidence = %+v, want the script and its line", q.Evidence)
	}
}

// A macOS app's release: an archive for macOS, a temporary keychain,
// notarization, and Xcode named by its path.
func TestJob_AMacOSAppRelease(t *testing.T) {
	fsys := fstest.MapFS{".github/workflows/mac-release.yml": {Data: []byte(`
on: push
jobs:
  build:
    runs-on: macos-latest
    env:
      DEVELOPER_DIR: /Applications/Xcode_27.app/Contents/Developer
    steps:
      - run: |
          export DEVELOPER_DIR=/Applications/Xcode_27.0.1.app/Contents/Developer
          security create-keychain -p "" build.keychain
          xcodebuild archive -scheme Mac -destination 'generic/platform=macOS' -archivePath out
          xcrun notarytool submit out.zip --wait
          xcrun stapler staple Mac.app
`)}}
	r := jobOf(t, fsys, ".github/workflows/mac-release.yml", "build", mac)
	if r.Kind != KindMacOSApp {
		t.Errorf("kind %s, want macos-app", r.Kind)
	}
	requireKeys(t, r, Xcode, MacOSApp, Codesign, Notarize)
	refuseKeys(t, r, IOSSDK)
	if q, _ := r.Get(Xcode); q.Version != "27" || q.VersionStrength != Declared {
		t.Errorf("xcode = %+v, want 27, declared by DEVELOPER_DIR", q)
	}
}

// An Android build from jobs.yaml: gradle, and the SDK and JDK its build file
// names.
func TestRegistryJob_AnAndroidBuild(t *testing.T) {
	fsys := fstest.MapFS{
		"app/build.gradle.kts": {Data: []byte("android {\n  compileSdk = 36\n}\nkotlin { jvmToolchain(17) }\n")},
	}
	reg, err := registry.Parse([]byte(`version: 1
jobs:
  android-smoke:
    runs_on: [linux, docker]
    steps:
      - name: build
        run: ./gradlew :app:assembleDebug
`))
	if err != nil {
		t.Fatal(err)
	}
	r := RegistryJob(fsys, reg.Jobs["android-smoke"])
	if r.Kind != KindAndroid || r.Platform != "Linux" {
		t.Errorf("kind %s on %s, want android on Linux", r.Kind, r.Platform)
	}
	if q, _ := r.Get(AndroidSDK); q.Version != "36" || q.VersionStrength != Declared {
		t.Errorf("android-sdk = %+v, want 36 from compileSdk", q)
	}
	if q, _ := r.Get(JDK); q.Version != "17" {
		t.Errorf("jdk = %+v, want 17 from jvmToolchain", q)
	}
}

// What a Linux job on Expo's cloud needs is Node: an eas build without
// --local is built on someone else's machine. With --local it is a native
// build here.
func TestJob_ExpoCloudAndLocalBuilds(t *testing.T) {
	fsys := fstest.MapFS{".github/workflows/mobile.yml": {Data: []byte(`
on: push
jobs:
  cloud:
    runs-on: ubuntu-latest
    steps:
      - run: bunx eas-cli build --platform ios --no-wait
      - run: bunx expo export --platform android
  local:
    runs-on: ubuntu-latest
    steps:
      - run: eas build --local --platform android
`)}}
	cloud := jobOf(t, fsys, ".github/workflows/mobile.yml", "cloud", linux)
	if cloud.Kind != KindLinux {
		t.Errorf("cloud kind %s, want linux", cloud.Kind)
	}
	refuseKeys(t, cloud, Xcode, IOSSDK, AndroidSDK)
	local := jobOf(t, fsys, ".github/workflows/mobile.yml", "local", linux)
	if local.Kind != KindAndroid {
		t.Errorf("local kind %s, want android", local.Kind)
	}
	requireKeys(t, local, AndroidSDK, JDK)
}

// The words are there and nothing runs them.
func TestJob_ToolchainWordsThatAreNotCommands(t *testing.T) {
	fsys := fstest.MapFS{".github/workflows/release.yml": {Data: []byte(`
on: push
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      # codesign happens on the Mac; this job only uploads
      - run: |
          echo "xcodebuild and gradlew ran elsewhere"
          cat > NOTES.md <<'EOF'
          docker build is not needed here
          EOF
          gh release upload v1 dist/*
`)}}
	r := jobOf(t, fsys, ".github/workflows/release.yml", "publish", linux)
	if r.Kind != KindLinux {
		t.Errorf("kind %s", r.Kind)
	}
	refuseKeys(t, r, Codesign, Xcode, Gradle, DockerDaemon)
}

// A step that only runs on the other platform says nothing about this leg.
func TestJob_AMatrixLegReadsOnlyItsOwnSteps(t *testing.T) {
	fsys := fstest.MapFS{".github/workflows/ci.yml": {Data: []byte(`
on: push
jobs:
  check:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - if: runner.os == 'Linux'
        run: sudo apt-get install -y minisign
      - run: go test ./...
`)}}
	m := jobOf(t, fsys, ".github/workflows/ci.yml", "check",
		Leg{Labels: []string{"macos"}, Matrix: map[string]any{"os": "macos-latest"}})
	refuseKeys(t, m, Apt, Sudo)
	requireKeys(t, m, Go)
	if m.Kind != KindMacOS {
		t.Errorf("macOS leg kind %s, want macos", m.Kind)
	}
	l := jobOf(t, fsys, ".github/workflows/ci.yml", "check",
		Leg{Labels: []string{"linux"}, Matrix: map[string]any{"os": "ubuntu-latest"}})
	requireKeys(t, l, Apt, Sudo, Go)
}

// A Docker daemon hides behind wrangler, and services: need one too.
func TestJob_DockerDaemons(t *testing.T) {
	fsys := fstest.MapFS{
		".github/workflows/deploy.yml": {Data: []byte(`
on: push
jobs:
  image:
    runs-on: ubuntu-latest
    steps:
      - run: bunx wrangler containers build -p workers/host
  workers:
    runs-on: ubuntu-latest
    steps:
      - uses: cloudflare/wrangler-action@v4
        with:
          command: deploy --config workers/host/wrangler.jsonc
  db:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
    steps:
      - run: bun test
  web:
    runs-on: ubuntu-latest
    steps:
      - uses: cloudflare/wrangler-action@v4
`)},
		"workers/host/wrangler.jsonc": {Data: []byte(`{"name":"host","containers":[{"class_name":"Box"}]}`)},
		"wrangler.jsonc":              {Data: []byte(`{"name":"web"}`)},
	}
	for job, want := range map[string]string{"image": KindLinuxDocker, "workers": KindLinuxDocker, "db": KindLinuxDocker, "web": KindLinux} {
		r := jobOf(t, fsys, ".github/workflows/deploy.yml", job, linux)
		if r.Kind != want {
			t.Errorf("%s: kind %s, want %s (%+v)", job, r.Kind, want, r.Requires)
		}
	}
	if q, _ := jobOf(t, fsys, ".github/workflows/deploy.yml", "workers", linux).Get(DockerDaemon); q.Strength != Inferred ||
		!strings.Contains(q.Evidence.Snippet, "declares containers") {
		t.Errorf("workers' daemon = %+v, want inferred from the config", q)
	}
}

// What cannot be read is said, not guessed.
func TestJob_WhatCannotBeRead(t *testing.T) {
	fsys := fstest.MapFS{".github/workflows/ci.yml": {Data: []byte(`
on: push
jobs:
  call:
    uses: ./.github/workflows/build.yml
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: someone/composite-thing@v1
      - run: ./scripts/missing.sh
      - run: eval "$BUILD"
`)}}
	if r := jobOf(t, fsys, ".github/workflows/ci.yml", "call", Leg{}); len(r.Unknown) != 1 || r.Kind != KindUnknown {
		t.Errorf("a reusable workflow caller = %+v", r)
	}
	r := jobOf(t, fsys, ".github/workflows/ci.yml", "build", linux)
	if len(r.Unknown) != 3 {
		t.Errorf("unknown = %+v, want the remote action, the missing script and the eval", r.Unknown)
	}
}

// A local composite action is read as the job's own steps.
func TestJob_LocalCompositeActions(t *testing.T) {
	fsys := fstest.MapFS{
		".github/workflows/ci.yml": {Data: []byte(`
on: push
jobs:
  b:
    runs-on: macos-latest
    steps:
      - uses: ./.github/actions/pods
`)},
		".github/actions/pods/action.yml": {Data: []byte("name: pods\nruns:\n  using: composite\n  steps:\n" +
			"    - run: bundle exec pod install\n      shell: bash\n")},
	}
	r := jobOf(t, fsys, ".github/workflows/ci.yml", "b", mac)
	requireKeys(t, r, CocoaPods, Bundler, Ruby)
	if r.Kind != KindXcode {
		t.Errorf("kind %s, want xcode", r.Kind)
	}
}

// An install runs each package's lifecycle scripts, and the CLIs they name
// run on Node: take-one's `bun install` ran the extension's `wxt prepare` on
// a Mac whose only Node was too old for it, and nothing said Node.
func TestJob_AnInstallWhoseWorkspaceRunsAPostinstall(t *testing.T) {
	workflow := `
on: pull_request
jobs:
  apple:
    runs-on: macos-26
    steps:
      - uses: oven-sh/setup-bun@v2
      - run: bun install --frozen-lockfile
`
	with := fstest.MapFS{
		".github/workflows/checks.yml":    {Data: []byte(workflow)},
		"package.json":                    {Data: []byte(`{"workspaces":["packages/*"]}`)},
		"packages/web/package.json":       {Data: []byte(`{"scripts":{"build":"vite build"}}`)},
		"packages/extension/package.json": {Data: []byte(`{"scripts":{"postinstall":"wxt prepare"}}`)},
	}
	r := jobOf(t, with, ".github/workflows/checks.yml", "apple", mac)
	q, ok := r.Get(Node)
	if !ok || q.Strength != Inferred || q.Evidence.File != "packages/extension/package.json" || q.Evidence.Where != "scripts.postinstall" {
		t.Errorf("node = %+v (found %v), want it inferred from the extension's postinstall", q, ok)
	}

	without := fstest.MapFS{
		".github/workflows/checks.yml": {Data: []byte(workflow)},
		"package.json":                 {Data: []byte(`{"workspaces":{"packages":["packages/*"]}}`)},
		"packages/web/package.json":    {Data: []byte(`{"scripts":{"build":"vite build"}}`)},
	}
	if _, ok := jobOf(t, without, ".github/workflows/checks.yml", "apple", mac).Get(Node); ok {
		t.Error("an install with no lifecycle scripts was said to need Node")
	}
}

// The shape of a preview build that was read as plain macOS: its gradlew sits
// behind env and a name with dots in it, and the prebuild beside it is named
// from where a sourced script had cd'd to.
func TestJob_AnAndroidBuildBehindEnvAndACd(t *testing.T) {
	fsys := fstest.MapFS{
		".github/workflows/preview.yml": {Data: []byte(`
on: push
jobs:
  android:
    runs-on: [macos, xcode]
    steps:
      - run: packages/app/ci/android/preview.sh
`)},
		"packages/app/ci/android/preview.sh": {Data: []byte(`#!/usr/bin/env bash
. ci/lib/env.sh
ci/prebuild.sh android
(
  cd android
  env \
    "ORG_GRADLE_PROJECT_android.injected.signing.store.file=$KEYSTORE" \
    ./gradlew :app:bundleRelease --no-daemon
)
`)},
		"packages/app/ci/prebuild.sh": {Data: []byte("CI=1 bunx expo prebuild --platform \"$platform\" --clean\n")},
	}
	r := jobOf(t, fsys, ".github/workflows/preview.yml", "android", mac)
	if r.Kind != KindAndroid {
		t.Errorf("kind = %q, requires %+v", r.Kind, r.Requires)
	}
	requireKeys(t, r, AndroidSDK, JDK, Gradle, Node)
}
