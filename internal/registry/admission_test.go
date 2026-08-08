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

	// Outside policy with approval enabled — proceeds, so that gate 9 can
	// resolve the commit gate 8 will park it with (amendment A27). Parking
	// here would produce a run with no commit to approve.
	if d := PreAdmit(req("myapp", "test", "feature/x"), p); !d.OK() {
		t.Errorf("gate 4 should defer to gate 8 when approval is possible, got %+v", d)
	}

	// Outside policy with approval disabled — rejected outright, with no
	// network touched. This is the half of the gate that is still decidable
	// here, and it is the half that keeps a hostile request cheap.
	d := PreAdmit(req("strict", "test", "feature/x"), p)
	if d.State != protocol.StateRejected || d.Code != protocol.ErrInvalidRef {
		t.Errorf("got %+v", d)
	}
}

// Gate 4 never parks. Approval is approval of a commit, and this gate runs
// before anything has been fetched, so there is no commit for it to name.
func TestPreAdmit_Gate4_NeverParks(t *testing.T) {
	p := testPolicy()
	for _, ref := range []string{"main", "feature/x", "refs/tags/v9", "release/1.2"} {
		for _, repo := range []string{"myapp", "strict"} {
			if d := PreAdmit(req(repo, "test", ref), p); d.State == protocol.StateAwaitingApproval {
				t.Errorf("PreAdmit(%s, %s) parked a run with no resolved commit: %+v", repo, ref, d)
			}
		}
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

	if d := AdmitRef("run_1", "myapp", "refs/heads/main", repo, NotApproved); !d.OK() {
		t.Errorf("refs/heads/main should be allowed by a policy of \"main\": %+v", d)
	}
	// A tag of the same name is a different object and needs approval.
	d := AdmitRef("run_1", "myapp", "refs/tags/main", repo, NotApproved)
	if d.State != protocol.StateAwaitingApproval {
		t.Errorf("refs/tags/main should need approval, got %+v", d)
	}
	if !strings.Contains(d.Msg, "latem approve run_1") {
		t.Errorf("message should name the command that unblocks it: %q", d.Msg)
	}
}

// The second half of INV-5: an approval bound to the commit in hand admits the
// run, and an approval of any other commit does not (amendment A27).
func TestAdmitRef_Approval(t *testing.T) {
	repo := RepoPolicy{AllowedRefs: []string{"main"}, RequireApprovalOtherwise: true}
	const ref = "refs/heads/feature/x"

	if d := AdmitRef("run_1", "myapp", ref, repo, ApprovedForThisCommit); !d.OK() {
		t.Errorf("an approved commit should run: %+v", d)
	}

	// The attack this exists to stop: approve a benign commit, force-push a
	// hostile one, and the run must not build the second.
	d := AdmitRef("run_1", "myapp", ref, repo, ApprovedForAnotherCommit)
	if d.State != protocol.StateAwaitingApproval {
		t.Fatalf("a moved ref should park again, got %+v", d)
	}
	if !strings.Contains(d.Msg, "moved") {
		t.Errorf("the message should say the ref moved, not repeat the policy line: %q", d.Msg)
	}
	if !strings.Contains(d.Msg, "latem approve run_1") {
		t.Errorf("message should name the command that unblocks it: %q", d.Msg)
	}

	// Approval is not a way around a repo that does not permit approval at all.
	closed := RepoPolicy{AllowedRefs: []string{"main"}}
	for _, a := range []Approval{NotApproved, ApprovedForAnotherCommit} {
		if d := AdmitRef("run_1", "myapp", ref, closed, a); d.State != protocol.StateRejected {
			t.Errorf("approval %v under a closed policy = %+v, want rejected", a, d)
		}
	}
}

func TestApprovalFor(t *testing.T) {
	const a = "0123456789abcdef0123456789abcdef01234567"
	const b = "fedcba9876543210fedcba9876543210fedcba98"

	tests := []struct {
		name              string
		approved, resolve string
		want              Approval
	}{
		{"nothing approved", "", a, NotApproved},
		{"same commit", a, a, ApprovedForThisCommit},
		{"the ref moved", a, b, ApprovedForAnotherCommit},
		// Two empty strings are equal, and must not be a match: that would
		// admit an unapproved run the moment a caller forgot to resolve one.
		{"both empty", "", "", NotApproved},
		{"approved but nothing resolved", a, "", ApprovedForAnotherCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApprovalFor(tc.approved, tc.resolve); got != tc.want {
				t.Errorf("ApprovalFor(%q, %q) = %v, want %v", tc.approved, tc.resolve, got, tc.want)
			}
		})
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
	d := AdmitRef("run_1", "myapp", "refs/tags/main", p.Repos["myapp"], NotApproved)
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
