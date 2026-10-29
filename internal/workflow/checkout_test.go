package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// checkoutScope is testScope with what a workflow run's checkout reads: the
// repository it belongs to, and a dispatch's inputs.
func checkoutScope() expr.Scope {
	sc := testScope()
	gh := sc.Contexts["github"].(map[string]any)
	gh["repository"] = "acme/app"
	sc.Contexts["inputs"] = map[string]any{"ref": ""}
	return sc
}

// A53: a checkout is a step the run loop performs — it needs no allowlisting,
// because nothing is fetched and nothing third-party runs, and it stays in
// the job because it now does something.
func TestCheckoutIsAStepTheRunLoopPerforms(t *testing.T) {
	f := &fakeFetcher{root: t.TempDir()}
	job, err := CompileWith(t.Context(),
		parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: actions/checkout@v5
      - name: again
        uses: actions/checkout@v5
        with:
          persist-credentials: false
          fetch-depth: 0
          fetch-tags: true
          clean: false
          submodules: false
          repository: ${{ github.repository }}
      - run: make
`),
		"b", checkoutScope(),
		// The refusing default: a checkout must work under it.
		Options{Policy: actions.DefaultPolicy(), Fetcher: f})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.Steps) != 3 || job.Steps[2].Run != "make" {
		t.Fatalf("steps = %+v, want both checkouts and make", job.Steps)
	}
	first, second := job.Steps[0], job.Steps[1]
	if first.Name != "Run actions/checkout@v5" || first.Checkout == nil || first.Run != "" {
		t.Fatalf("first step = %+v, want a checkout named as GitHub names it", first)
	}
	if c := first.Checkout; !c.PersistCredentials || !c.Clean || c.FetchDepth != 1 || c.Ref != "" || c.RefLate != nil || c.Token != nil {
		t.Errorf("defaults = %+v, want GitHub's: persist, clean, depth 1, the run's commit, the job's token", c)
	}
	if c := second.Checkout; c.PersistCredentials || c.Clean || c.FetchDepth != 0 || !c.FetchTags {
		t.Errorf("written values = %+v", c)
	}
	if len(f.fetched) != 0 {
		t.Errorf("a checkout fetched something: %v", f.fetched)
	}
	if job.RequiresIsolation {
		t.Error("a checkout is not third-party code")
	}
}

// The deploy workflows' three shapes: a ref known when the job starts, one
// known only after an earlier step, and the job's token named as a secret.
func TestCheckoutRefAndToken(t *testing.T) {
	job, err := CompileWith(t.Context(),
		parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: actions/checkout@v5
        with:
          ref: ${{ inputs.ref || 'main' }}
          token: ${{ secrets.DEPLOY_TOKEN }}
      - id: bump
        run: echo "sha=abc" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@v5
        with:
          ref: ${{ steps.bump.outputs.sha }}
`),
		"b", checkoutScope(), Options{Policy: actions.DefaultPolicy()})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	early, late := job.Steps[0].Checkout, job.Steps[2].Checkout
	if early.Ref != "main" || early.RefLate != nil {
		t.Errorf("a ref known at admission = %+v, want main, resolved now", early)
	}
	if early.Token == nil || !strings.Contains(early.Token.Source(), "secrets.DEPLOY_TOKEN") {
		t.Errorf("token = %v, want the secret, bound as the step runs", early.Token)
	}
	if late.Ref != "" || late.RefLate == nil || !slices.Contains(late.RefLate.References(), "bump") {
		t.Errorf("a ref known only after step bump = %+v", late)
	}
	if !slices.Contains(job.Secrets, "DEPLOY_TOKEN") {
		t.Errorf("secrets = %v, want DEPLOY_TOKEN resolved for the run", job.Secrets)
	}
}

// What a checkout cannot honour is refused by name, with the input that made
// it impossible — because silently accepting it is the failure nobody traces
// back.
func TestCheckoutRefusesWhatItCannotHonour(t *testing.T) {
	cases := map[string]struct{ with, want string }{
		"another repo":         {"repository: other/thing", "another repository"},
		"a subdirectory":       {"path: vendor/thing", "workspace root"},
		"submodules":           {"submodules: true", "submodules: true is not built"},
		"an unknown input":     {"sparse-checkout: src", "sparse-checkout (not an input"},
		"a depth that is not":  {"fetch-depth: many", "whole number"},
		"a written token":      {"token: ghp_0123456789abcdefghij", "belongs in a secret"},
		"another server":       {"github-server-url: https://ghe.example.com", "not github.com"},
		"a boolean that isn't": {"clean: maybe", "not true or false"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := CompileWith(t.Context(),
				parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: actions/checkout@v5\n        with:\n          "+c.with+"\n      - run: make\n"),
				"b", checkoutScope(), Options{Policy: actions.DefaultPolicy()})
			if err == nil {
				t.Fatalf("with %q was accepted", c.with)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %v does not say %q", err, c.want)
			}
		})
	}
}
