package workflow

import (
	"encoding/json"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
)

// latem's own ci.yml, in miniature: setup-go on Linux only, and a matrix over
// both platforms.
const platformMatrix = `
jobs:
  b:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - id: go
        uses: acme/setup-go@` + pinned + `
        if: runner.os == 'Linux'
      - uses: acme/setup-go@` + pinned + `
        if: ${{ startsWith(matrix.os, 'ubuntu') }}
      - run: go test ./...
`

const nodeAction = "runs:\n  using: node20\n  main: index.js\n"

// A step whose if: reads only runner.os and the matrix, and is false on the
// platform the leg's labels settle, is decided at compile (A58): its action is
// never fetched and asks for no isolation, and it stays, to be skipped when
// reached, as GitHub reports it.
func TestAStepForAnotherPlatformIsNeverFetched(t *testing.T) {
	ref, _ := actions.ParseRef("acme/setup-go@" + pinned)

	macF := newFetcher(t, ref, nodeAction)
	mac, err := CompileWith(t.Context(), parse(t, platformMatrix), "b", testScope(), Options{
		Policy: actions.Policy{Allowed: []string{"*/*"}}, Fetcher: macF,
		Matrix: json.RawMessage(`{"os":"macos-latest"}`),
	})
	if err != nil {
		t.Fatalf("the macOS leg: %v", err)
	}
	if len(macF.fetched) != 0 || mac.RequiresIsolation {
		t.Errorf("the macOS leg fetched %v (isolation %v); its setup-go steps are Linux's", macF.fetched, mac.RequiresIsolation)
	}
	// The runner.os step stays, with its id, name and if:, to be skipped when
	// reached. The matrix-only one is answered at admission and dropped, as
	// any step whose if: the admission scope answers false always was.
	if len(mac.Steps) != 2 || mac.Steps[0].ID != "go" || mac.Steps[0].Name != "Run acme/setup-go@"+pinned || mac.Steps[0].If == nil {
		t.Errorf("steps = %+v", mac.Steps)
	}

	linuxF := newFetcher(t, ref, nodeAction)
	linux, err := CompileWith(t.Context(), parse(t, platformMatrix), "b", testScope(), Options{
		Policy: actions.Policy{Allowed: []string{"*/*"}}, Fetcher: linuxF,
		Matrix: json.RawMessage(`{"os":"ubuntu-latest"}`),
	})
	if err != nil {
		t.Fatalf("the Linux leg: %v", err)
	}
	if len(linuxF.fetched) != 2 || !linux.RequiresIsolation {
		t.Errorf("the Linux leg fetched %v (isolation %v), want both setup-go steps", linuxF.fetched, linux.RequiresIsolation)
	}
}

// Only a condition this side of the run can answer is decided here.
func TestOnlyAPlatformConditionIsDecidedAtCompile(t *testing.T) {
	ref, _ := actions.ParseRef("acme/setup-go@" + pinned)
	for name, cond := range map[string]string{
		"it reads env":            "runner.os == 'Linux' && env.GO",
		"a status function":       "always() && runner.os == 'Linux'",
		"another runner property": "runner.arch == 'X64'",
		"it is true here":         "runner.os == 'macOS'",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFetcher(t, ref, nodeAction)
			_, err := CompileWith(t.Context(), parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: acme/setup-go@`+pinned+`
        if: `+cond+`
`), "b", testScope(), Options{Policy: actions.Policy{Allowed: []string{"*/*"}, AllowUnisolated: true}, Fetcher: f})
			if err != nil {
				t.Fatal(err)
			}
			if len(f.fetched) != 1 {
				t.Errorf("fetched %v: %q is the run's to answer", f.fetched, cond)
			}
		})
	}
}

func TestOSOfLabels(t *testing.T) {
	for labels, want := range map[string]string{"macos": "macOS", "linux": "Linux", "": "", "gpu": ""} {
		var ls []string
		if labels != "" {
			ls = []string{labels}
		}
		if got := osOfLabels(ls); got != want {
			t.Errorf("osOfLabels(%v) = %q, want %q", ls, got, want)
		}
	}
	if got := osOfLabels([]string{"macos", "linux"}); got != "" {
		t.Errorf("both platforms = %q, want unknown", got)
	}
}
