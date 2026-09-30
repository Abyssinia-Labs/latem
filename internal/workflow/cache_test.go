package workflow

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// regexpCacheName is the registry's own rule, restated so a derived name that
// would fail validation fails here first.
var regexpCacheName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func protocolRequest(job string) protocol.RunRequest {
	return protocol.RunRequest{Job: job, Ref: "refs/heads/main"}
}

func compileDefault(t *testing.T, src string) (*registry.Job, error) {
	t.Helper()
	// The refusing default policy: the cache shim, like checkout, must work
	// on a machine that has allowed no actions at all.
	return CompileWith(t.Context(), parse(t, src), "b", testScope(), Options{Policy: actions.DefaultPolicy()})
}

// actions/cache becomes a mount, the step disappears, and the registry built
// from the job is one the existing validation and admission accept.
func TestCacheShimBecomesADeclaredMount(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: actions/cache@v4
        id: npm
        with:
          path: |
            ~/.npm
            /opt/tools/cache
          key: ${{ runner.os }}-npm-${{ hashFiles('**/package-lock.json') }}
          restore-keys: ${{ runner.os }}-npm-
      - if: steps.npm.outputs.cache-hit != 'true'
        run: npm ci
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.Steps) != 1 || job.Steps[0].Run != "npm ci" {
		t.Fatalf("steps = %+v, want only npm ci", job.Steps)
	}
	// cache-hit is 'false' at compile time, so the condition folded away
	// rather than being carried to the run loop.
	if job.Steps[0].If != nil {
		t.Errorf("the cache-hit condition should have been answered at admission: %s", job.Steps[0].If.Source())
	}

	if len(job.CacheDecls) != 2 || len(job.Caches) != 2 {
		t.Fatalf("decls = %+v, caches = %v", job.CacheDecls, job.Caches)
	}
	if job.CacheDecls[0].Path != "~/.npm" || job.CacheDecls[1].Path != "/opt/tools/cache" {
		t.Errorf("paths = %+v", job.CacheDecls)
	}

	reg := RegistryFor("b", job)
	if err := reg.Validate(); err != nil {
		t.Fatalf("the registry must validate: %v", err)
	}
	res, d := reg.AdmitOn(protocolRequest("b"), registry.Runner{Labels: []string{"linux"}, Executor: "docker", Isolates: true})
	if !d.OK() {
		t.Fatalf("admit: %+v", d)
	}
	if len(res.Caches) != 2 {
		t.Errorf("admission should carry both caches to Prepare: %+v", res.Caches)
	}
}

// Named from the path alone, so every job caching ~/.npm on a runner shares one
// mount — and two cache steps naming the same path declare it once.
func TestCacheNamesComeFromThePathAlone(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: actions/cache/restore@v4
        with: {path: ~/.npm, key: a}
      - run: npm ci
      - uses: actions/cache/save@v4
        with: {path: ~/.npm, key: b}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.CacheDecls) != 1 {
		t.Fatalf("one path should be one cache: %+v", job.CacheDecls)
	}
	if got := cacheNameFor("~/.npm"); got != job.CacheDecls[0].Name {
		t.Errorf("name = %q, want the path's", job.CacheDecls[0].Name)
	}
	if cacheNameFor("~/.npm") == cacheNameFor("~/.cache/npm") {
		t.Error("different paths must be different caches")
	}
	for _, p := range []string{"~/.npm", "/very/long/" + strings.Repeat("segment/", 20) + "leaf", "~"} {
		if n := cacheNameFor(p); !regexpCacheName.MatchString(n) {
			t.Errorf("cacheNameFor(%q) = %q does not satisfy the registry's pattern", p, n)
		}
	}
}

// Each refusal is something a mount cannot be, named at compile time rather
// than left to fail as E_CACHE partway into preparing.
func TestCacheShimRefusals(t *testing.T) {
	cases := map[string]struct{ with, want string }{
		"inside the workspace":    {"path: node_modules", "shadow the checkout"},
		"a pattern":               {"path: ~/.cache/*", "pattern"},
		"an exclusion":            {"path: |\n            ~/.npm\n            !~/.npm/_logs", "exclusion"},
		"an environment variable": {"path: $HOME/.npm", "environment variable"},
		"a late path":             {"path: ${{ steps.store.outputs.path }}", "mounted before the first step"},
		"exact-key semantics":     {"path: ~/.npm\n          fail-on-cache-miss: true", "no keys to answer with"},
		"lookup only":             {"path: ~/.npm\n          lookup-only: true", "no keys to answer with"},
		"an unknown input":        {"path: ~/.npm\n          compression: zstd", "does not know"},
		"no path at all":          {"key: x", "requires the input"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - id: store\n        run: echo\n" +
				"      - uses: actions/cache@v4\n        with:\n          " + c.with + "\n"
			_, err := compileDefault(t, src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// An admission-time expression in the path is resolved; the primary key output
// stays whatever expression the workflow wrote, and so is exactly as late as
// that expression is.
func TestCachePathExpressionsAndOutputs(t *testing.T) {
	job, err := compileDefault(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: actions/cache@v4
        id: c
        with:
          path: /cache/${{ github.sha }}
          key: ${{ runner.os }}-x
      - run: echo ${{ steps.c.outputs.cache-primary-key }} ${{ steps.c.outputs.cache-matched-key }}
`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if job.CacheDecls[0].Path != "/cache/deadbeef" {
		t.Errorf("path = %q", job.CacheDecls[0].Path)
	}
	st := job.Steps[0]
	// runner.os makes the primary key late; the matched key is a literal.
	if len(st.Late) != 1 {
		t.Fatalf("want one late value (the primary key): late=%+v env=%+v", st.Late, st.Env)
	}
	for _, v := range st.Env {
		if v != "" {
			t.Errorf("the matched key should be empty, got %q", v)
		}
	}
	got, err := func() (string, error) {
		for _, v := range st.Late {
			return v.Resolve(registry.StepScope{Runner: registry.RunnerFacts{OS: "linux"}})
		}
		return "", nil
	}()
	if err != nil || got != "Linux-x" {
		t.Errorf("primary key = %q, %v", got, err)
	}

	// An output actions/cache does not have is still refused, not read as empty.
	_, err = compileDefault(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: actions/cache@v4\n        id: c\n        with: {path: ~/.npm, key: k}\n      - run: echo ${{ steps.c.outputs.nope }}\n")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("an undeclared output should be refused: %v", err)
	}
}

// A cache inside a composite action is declared for the job like any other.
func TestCacheInsideACompositeAction(t *testing.T) {
	job, err := CompileWith(t.Context(), parse(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: ./setup
        id: s
      - if: steps.s.outputs.hit != 'true'
        run: make deps
`), "b", testScope(), Options{Policy: actions.DefaultPolicy(), Actions: mapLoader{"setup": `
outputs:
  hit:
    value: ${{ steps.c.outputs.cache-hit }}
runs:
  using: composite
  steps:
    - uses: actions/cache@v4
      id: c
      with: {path: ~/.gradle, key: g}
`}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.CacheDecls) != 1 || job.CacheDecls[0].Path != "~/.gradle" {
		t.Errorf("decls = %+v", job.CacheDecls)
	}
	// The composite's output was the nested cache-hit, so it folded too.
	if len(job.Steps) != 1 || job.Steps[0].If != nil {
		t.Errorf("steps = %+v", job.Steps)
	}
}
