package registry

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func testPolicy() Policy {
	return Policy{
		Repos: map[string]RepoPolicy{
			"myapp": {
				AllowedRefs:              []string{"main", "release/*"},
				RequireApprovalOtherwise: true,
			},
			"strict": {
				AllowedRefs:              []string{"main"},
				RequireApprovalOtherwise: false,
			},
		},
		FreeGB:    100,
		MinFreeGB: 40,
	}
}

func req(repo, job, ref string) protocol.RunRequest {
	return protocol.RunRequest{ID: "run_1", Repo: repo, Job: job, Ref: ref}
}

func TestPreAdmit_Gate1_UnknownRepo(t *testing.T) {
	d := PreAdmit(req("nope", "test", "main"), testPolicy())
	if d.Code != protocol.ErrUnknownRepo || d.State != protocol.StateRejected {
		t.Fatalf("got %+v", d)
	}
}

func TestPreAdmit_Gate2_InvalidRef(t *testing.T) {
	d := PreAdmit(req("myapp", "test", "--upload-pack=/bin/sh"), testPolicy())
	if d.Code != protocol.ErrInvalidRef {
		t.Fatalf("got %+v", d)
	}
}

func TestPreAdmit_Gate3_InputSanity(t *testing.T) {
	p := testPolicy()

	tooMany := req("myapp", "test", "main")
	tooMany.Inputs = map[string]string{}
	for i := 0; i <= MaxInputKeys; i++ {
		tooMany.Inputs["K"+strings.Repeat("x", i)] = "v"
	}
	if d := PreAdmit(tooMany, p); d.Code != protocol.ErrBadInput {
		t.Errorf("too many inputs: got %+v", d)
	}

	badKey := req("myapp", "test", "main")
	badKey.Inputs = map[string]string{"not-an-ident": "v"}
	if d := PreAdmit(badKey, p); d.Code != protocol.ErrBadInput {
		t.Errorf("bad key: got %+v", d)
	}

	huge := req("myapp", "test", "main")
	huge.Inputs = map[string]string{"NOTES": strings.Repeat("x", MaxInputValueBytes+1)}
	if d := PreAdmit(huge, p); d.Code != protocol.ErrBadInput {
		t.Errorf("oversized value: got %+v", d)
	}
}

func TestPreAdmit_Gate4_RefPolicy(t *testing.T) {
	p := testPolicy()

	// Inside policy — proceeds.
	for _, ref := range []string{"main", "release/1.2"} {
		if d := PreAdmit(req("myapp", "test", ref), p); !d.OK() {
			t.Errorf("%q should proceed, got %+v", ref, d)
		}
	}

	// Outside policy with approval enabled — parks rather than fails.
	d := PreAdmit(req("myapp", "test", "feature/x"), p)
	if d.State != protocol.StateAwaitingApproval {
		t.Fatalf("got %+v", d)
	}
	if !strings.Contains(d.Msg, "latem approve run_1") {
		t.Errorf("message should name the command that unblocks it: %q", d.Msg)
	}

	// Outside policy with approval disabled — rejected outright.
	if d := PreAdmit(req("strict", "test", "feature/x"), p); d.State != protocol.StateRejected {
		t.Errorf("got %+v", d)
	}
}

func TestPreAdmit_Gate5_Disk(t *testing.T) {
	p := testPolicy()
	p.FreeGB = 10
	if d := PreAdmit(req("myapp", "test", "main"), p); d.Code != protocol.ErrDisk {
		t.Fatalf("got %+v", d)
	}
}

// Ordering is the point of the chain: the cheapest, most specific rejection
// wins, so a hostile request is refused for the first reason it hits rather
// than whichever check happened to run.
func TestPreAdmit_GateOrdering(t *testing.T) {
	p := testPolicy()
	p.FreeGB = 1 // gate 5 would also fail

	// Unknown repo AND a hostile ref AND no disk: gate 1 answers.
	bad := req("nope", "test", "--upload-pack=/bin/sh")
	bad.Inputs = map[string]string{"bad-key": "v"}
	if d := PreAdmit(bad, p); d.Code != protocol.ErrUnknownRepo {
		t.Errorf("want gate 1 to answer first, got %s", d.Code)
	}

	// Known repo, hostile ref, bad inputs: gate 2 answers.
	bad2 := req("myapp", "test", "$(id)")
	bad2.Inputs = map[string]string{"bad-key": "v"}
	if d := PreAdmit(bad2, p); d.Code != protocol.ErrInvalidRef {
		t.Errorf("want gate 2 to answer, got %s", d.Code)
	}
}

// Gate 8, the authoritative check, runs against the normalized name. This is
// the half of finding F5 that gate 4 cannot decide.
func TestAdmitRef_Authoritative(t *testing.T) {
	repo := RepoPolicy{AllowedRefs: []string{"main"}, RequireApprovalOtherwise: true}

	if d := AdmitRef("run_1", "myapp", "refs/heads/main", repo); !d.OK() {
		t.Errorf("refs/heads/main should be allowed by a policy of \"main\": %+v", d)
	}
	// A tag of the same name is a different object and needs approval.
	if d := AdmitRef("run_1", "myapp", "refs/tags/main", repo); d.State != protocol.StateAwaitingApproval {
		t.Errorf("refs/tags/main should need approval, got %+v", d)
	}
}

// Gate 4 admits "main" optimistically because it could be a branch. If it
// turns out to be a tag, gate 8 catches it. This is the pair working together.
func TestGate4And8_TogetherCatchATagPretendingToBeABranch(t *testing.T) {
	p := testPolicy()

	// Gate 4 lets it through: refs/heads/main would have matched.
	if d := PreAdmit(req("myapp", "test", "main"), p); !d.OK() {
		t.Fatalf("gate 4 should admit %q optimistically: %+v", "main", d)
	}
	// Gate 8, after normalization discovered it is actually a tag, does not.
	d := AdmitRef("run_1", "myapp", "refs/tags/main", p.Repos["myapp"])
	if d.OK() {
		t.Error("gate 8 should refuse a tag under a branch policy")
	}
}

func admitFixture(t *testing.T) *Registry {
	t.Helper()
	r, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAdmit_Gate12_UnknownJob(t *testing.T) {
	r := admitFixture(t)
	_, d := r.Admit(req("myapp", "nope", "main"), []string{"linux", "docker"})
	if d.Code != protocol.ErrUnknownJob {
		t.Fatalf("got %+v", d)
	}
	// Naming what does exist turns a dead end into a fix.
	if !strings.Contains(d.Msg, "ios-release") {
		t.Errorf("message should list known jobs: %q", d.Msg)
	}
}

func TestAdmit_Gate13_LabelMatch(t *testing.T) {
	r := admitFixture(t)

	// Runner carries every required label, plus extras — allowed.
	if _, d := r.Admit(req("myapp", "test", "main"), []string{"linux", "docker", "amd64"}); !d.OK() {
		t.Errorf("superset of labels should match: %+v", d)
	}

	// Missing one label — E_NO_RUNNER, naming which.
	_, d := r.Admit(req("myapp", "test", "main"), []string{"linux"})
	if d.Code != protocol.ErrNoRunner {
		t.Fatalf("got %+v", d)
	}
	if !strings.Contains(d.Msg, "docker") {
		t.Errorf("message should name the missing label: %q", d.Msg)
	}

	// An iOS job must never land on a Linux runner.
	if _, d := r.Admit(req("myapp", "ios-release", "main"), []string{"linux", "docker"}); d.Code != protocol.ErrNoRunner {
		t.Errorf("ios-release should not run on a linux runner: %+v", d)
	}
}

func TestAdmit_Gate14_Inputs(t *testing.T) {
	r := admitFixture(t)
	labels := []string{"macos", "xcode"}

	rq := req("myapp", "ios-release", "main")
	rq.Inputs = map[string]string{"channel": "beta"} // not in the enum
	if _, d := r.Admit(rq, labels); d.Code != protocol.ErrBadInput {
		t.Fatalf("got %+v", d)
	}

	rq.Inputs = map[string]string{"undeclared": "x"}
	if _, d := r.Admit(rq, labels); d.Code != protocol.ErrBadInput {
		t.Fatalf("undeclared input: got %+v", d)
	}
}

func TestAdmit_ResolvesEverythingARunNeeds(t *testing.T) {
	r := admitFixture(t)

	rq := req("myapp", "ios-release", "main")
	rq.Inputs = map[string]string{"channel": "external", "notes": "ship it"}

	res, d := r.Admit(rq, []string{"macos", "xcode", "arm64"})
	if !d.OK() {
		t.Fatalf("admission failed: %+v", d)
	}

	// Inputs coerced, defaults filled.
	if res.Inputs["channel"] != "external" || res.Inputs["notes"] != "ship it" {
		t.Errorf("inputs = %v", res.Inputs)
	}

	// Env substituted; run left literal.
	build := res.Steps[1]
	if build.Env["RELEASE_CHANNEL"] != "external" {
		t.Errorf("RELEASE_CHANNEL = %q", build.Env["RELEASE_CHANNEL"])
	}
	if build.Env["RELEASE_NOTES"] != "ship it" {
		t.Errorf("RELEASE_NOTES = %q", build.Env["RELEASE_NOTES"])
	}
	if build.Run != "./ci/ios/build.sh" {
		t.Errorf("run should be literal, got %q", build.Run)
	}

	// Caches resolved to declarations with their mount paths.
	if len(res.Caches) != 3 {
		t.Fatalf("got %d caches", len(res.Caches))
	}
	for _, c := range res.Caches {
		if c.Path == "" {
			t.Errorf("cache %q has no path", c.Name)
		}
	}

	if res.ConcurrencyGroup != "ios" {
		t.Errorf("concurrency_group = %q", res.ConcurrencyGroup)
	}
	if len(res.Secrets) != 2 {
		t.Errorf("secrets = %v", res.Secrets)
	}
	if res.Shell != "bash" {
		t.Errorf("shell = %q", res.Shell)
	}
}

// Substitution reaches env values only. Nothing a broker supplies may end up
// inside a command string (INV-3), and this is the assertion that would fail
// if someone later "helpfully" extended interpolation to run.
func TestAdmit_InputsNeverReachRun(t *testing.T) {
	r, err := Parse([]byte(`
version: 1
jobs:
  test:
    runs_on: [linux]
    inputs:
      TARGET: {type: string, default: "prod"}
    steps:
      - run: ./deploy.sh
        env:
          TARGET: ${input.TARGET}
`))
	if err != nil {
		t.Fatal(err)
	}

	rq := req("myapp", "test", "main")
	rq.Inputs = map[string]string{"TARGET": "; rm -rf /"}

	res, d := r.Admit(rq, []string{"linux"})
	if !d.OK() {
		t.Fatalf("admission failed: %+v", d)
	}
	if res.Steps[0].Run != "./deploy.sh" {
		t.Fatalf("run was modified: %q", res.Steps[0].Run)
	}
	// The hostile value is confined to the environment, where it is inert:
	// it is passed to the executor as an env entry, never re-parsed by a shell.
	if res.Steps[0].Env["TARGET"] != "; rm -rf /" {
		t.Errorf("env value = %q", res.Steps[0].Env["TARGET"])
	}
}
