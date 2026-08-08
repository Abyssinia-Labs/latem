package registry

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The example from RUNNER_SPEC §9, verbatim. If the spec's own illustration
// does not parse, one of the two is wrong and it matters which.
const specExample = `
version: 1

defaults:
  timeout: 30m
  shell: bash

caches:
  - name: pnpm
    path: /home/runner/.pnpm-store
  - name: gradle
    path: /home/runner/.gradle
  - name: cocoapods
    path: ~/Library/Caches/CocoaPods
  - name: derived-data
    path: ~/Library/Developer/Xcode/DerivedData

jobs:
  test:
    description: Unit tests
    runs_on: [linux, docker]
    image: ghcr.io/me/ci-base:20
    timeout: 15m
    caches: [pnpm]
    steps:
      - name: Install
        run: ./ci/install.sh
        timeout: 5m
      - name: Test
        run: ./ci/test.sh
        timeout: 10m

  ios-release:
    description: Build and upload an iOS release to TestFlight
    runs_on: [macos, xcode]
    timeout: 90m
    caches: [cocoapods, derived-data, pnpm]
    concurrency_group: ios
    inputs:
      channel:
        type: enum
        values: [internal, external]
        default: internal
      notes:
        type: string
        max_length: 500
        default: ""
    secrets:
      - MATCH_PASSWORD
      - APP_STORE_CONNECT_KEY_ID
    artifacts:
      - path: build/*.ipa
        retain: 7d
    steps:
      - name: Unlock keychain
        run: ./ci/ios/unlock-keychain.sh
        timeout: 2m
      - name: Build and upload
        run: ./ci/ios/build.sh
        timeout: 75m
        env:
          RELEASE_CHANNEL: ${input.channel}
          RELEASE_NOTES: ${input.notes}
`

func TestParse_SpecExample(t *testing.T) {
	r, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatalf("the spec's own §9 example does not parse: %v", err)
	}

	if len(r.Jobs) != 2 {
		t.Fatalf("got %d jobs", len(r.Jobs))
	}
	if r.Defaults.Shell != "bash" {
		t.Errorf("shell = %q", r.Defaults.Shell)
	}

	ios := r.Jobs["ios-release"]
	if ios.ConcurrencyGroup != "ios" {
		t.Errorf("concurrency_group = %q", ios.ConcurrencyGroup)
	}
	if got := r.JobTimeout(ios); got != 90*time.Minute {
		t.Errorf("timeout = %v", got)
	}
	// retain: 7d — the day unit Go's own parser rejects, which is why
	// protocol.Duration had to grow one.
	if got := ios.Artifacts[0].Retain.Duration(); got != 7*24*time.Hour {
		t.Errorf("retain = %v, want 168h", got)
	}
	if got := ios.Steps[1].Timeout.Duration(); got != 75*time.Minute {
		t.Errorf("step timeout = %v", got)
	}
}

// A typo in a CI file should fail the run that introduced it rather than
// silently doing something other than what was written.
func TestParse_StrictDecoding(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown top-level key": `
version: 1
jobz:
  test: {}
`,
		"unknown job key": `
version: 1
jobs:
  test:
    runs_on: [linux]
    runz_on: [linux]
    steps: [{run: "true"}]
`,
		"unknown step key": `
version: 1
jobs:
  test:
    runs_on: [linux]
    steps:
      - run: "true"
        shell: zsh
`,
		"actions-style uses": `
version: 1
jobs:
  test:
    runs_on: [linux]
    steps:
      - uses: actions/checkout@v4
`,
		"if condition": `
version: 1
jobs:
  test:
    runs_on: [linux]
    steps:
      - run: "true"
        if: success()
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Error("accepted an unknown field")
			}
		})
	}
}

func TestValidate_Rejects(t *testing.T) {
	tests := map[string]string{
		"wrong version": `
version: 2
jobs: {test: {runs_on: [linux], steps: [{run: "true"}]}}
`,
		"no jobs": `
version: 1
jobs: {}
`,
		"job without runs_on": `
version: 1
jobs: {test: {steps: [{run: "true"}]}}
`,
		"job without steps": `
version: 1
jobs: {test: {runs_on: [linux]}}
`,
		"step without run": `
version: 1
jobs: {test: {runs_on: [linux], steps: [{name: empty}]}}
`,
		"undeclared cache": `
version: 1
jobs: {test: {runs_on: [linux], caches: [nope], steps: [{run: "true"}]}}
`,
		"duplicate cache": `
version: 1
caches:
  - {name: pnpm, path: /a}
  - {name: pnpm, path: /b}
jobs: {test: {runs_on: [linux], steps: [{run: "true"}]}}
`,
		"cache without path": `
version: 1
caches: [{name: pnpm}]
jobs: {test: {runs_on: [linux], caches: [pnpm], steps: [{run: "true"}]}}
`,
		"interpolation in run": `
version: 1
jobs:
  test:
    runs_on: [linux]
    inputs: {CHANNEL: {type: string}}
    steps: [{run: "./build.sh ${input.CHANNEL}"}]
`,
		"undeclared input in env": `
version: 1
jobs:
  test:
    runs_on: [linux]
    steps: [{run: "true", env: {C: "${input.NOPE}"}}]
`,
		"secret reference in env": `
version: 1
jobs:
  test:
    runs_on: [linux]
    steps: [{run: "true", env: {P: "${secret.MATCH_PASSWORD}"}}]
`,
		"reserved env prefix": `
version: 1
jobs:
  test:
    runs_on: [linux]
    steps: [{run: "true", env: {LATEM_SHA: "fake"}}]
`,
		"absolute artifact path": `
version: 1
jobs:
  test:
    runs_on: [linux]
    artifacts: [{path: /etc/passwd}]
    steps: [{run: "true"}]
`,
		"artifact escaping workspace": `
version: 1
jobs:
  test:
    runs_on: [linux]
    artifacts: [{path: "../../etc/passwd"}]
    steps: [{run: "true"}]
`,
		"bad label": `
version: 1
jobs: {test: {runs_on: ["Linux Box"], steps: [{run: "true"}]}}
`,
		"bad secret name": `
version: 1
jobs: {test: {runs_on: [linux], secrets: ["my-secret"], steps: [{run: "true"}]}}
`,
		"duplicate secret": `
version: 1
jobs: {test: {runs_on: [linux], secrets: [A, A], steps: [{run: "true"}]}}
`,
	}

	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("accepted an invalid registry")
			}
			if got := CodeOf(err); got != protocol.ErrBadRegistry {
				t.Errorf("code = %s, want %s", got, protocol.ErrBadRegistry)
			}
		})
	}
}

// Someone fixing a jobs.yaml should see everything wrong with it in one pass,
// not discover the next error after each round trip through a build.
func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
jobs:
  a: {steps: [{run: "true"}]}
  b: {runs_on: [linux]}
  c: {runs_on: [linux], caches: [nope], steps: [{run: "true"}]}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"runs_on is required", "at least one step", "not declared"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q; got:\n%s", want, msg)
		}
	}
}

func TestValidate_DefaultsAreApplied(t *testing.T) {
	r, err := Parse([]byte(`
version: 1
jobs: {test: {runs_on: [linux], steps: [{run: "true"}]}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Defaults.Shell != DefaultShell {
		t.Errorf("shell = %q, want %q", r.Defaults.Shell, DefaultShell)
	}
	if got := r.JobTimeout(r.Jobs["test"]); got != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", got, DefaultTimeout)
	}
}

// A job timeout overrides the registry default; an absent one inherits it.
func TestJobTimeout_Precedence(t *testing.T) {
	r, err := Parse([]byte(`
version: 1
defaults: {timeout: 10m}
jobs:
  short: {runs_on: [linux], steps: [{run: "true"}]}
  long:  {runs_on: [linux], timeout: 2h, steps: [{run: "true"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.JobTimeout(r.Jobs["short"]); got != 10*time.Minute {
		t.Errorf("inherited timeout = %v", got)
	}
	if got := r.JobTimeout(r.Jobs["long"]); got != 2*time.Hour {
		t.Errorf("overridden timeout = %v", got)
	}
}

func TestParse_RejectsMalformedYAML(t *testing.T) {
	for _, doc := range []string{
		"version: 1\n  bad indent",
		"{{{",
		"version: [1]",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("accepted malformed YAML: %q", doc)
		}
	}
}

// Duplicate keys are how one job silently replaces another. sigs.k8s.io/yaml
// in strict mode catches it; this pins that we depend on that behaviour.
func TestParse_RejectsDuplicateKeys(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
jobs:
  test: {runs_on: [linux], steps: [{run: "one"}]}
  test: {runs_on: [linux], steps: [{run: "two"}]}
`))
	if err == nil {
		t.Error("accepted a duplicate job key")
	}
}
