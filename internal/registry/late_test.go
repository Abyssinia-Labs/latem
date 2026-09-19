package registry

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeLate is the smallest LateValue: a fixed answer and a fixed set of
// references. The registry never evaluates one during validation, so the
// answer is irrelevant here and the references are everything.
type fakeLate struct {
	src  string
	refs []string
	val  string
}

func (f fakeLate) Resolve(StepScope) (string, error) { return f.val, nil }
func (f fakeLate) References() []string              { return f.refs }
func (f fakeLate) Source() string                    { return f.src }

func lateJob(steps ...Step) *Registry {
	return &Registry{
		Version: SchemaVersion,
		Jobs: map[string]*Job{
			"j": {RunsOn: []string{"macos"}, Steps: steps},
		},
	}
}

func step(id, run string) Step { return Step{ID: id, Run: run} }

func TestLateReferenceToAnEarlierStepIsAccepted(t *testing.T) {
	r := lateJob(
		step("build", "make"),
		Step{Run: `echo "$GHA_EXPR_1"`, Late: map[string]LateValue{
			"GHA_EXPR_1": fakeLate{src: "steps.build.outputs.sha", refs: []string{"build"}},
		}},
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("a backward reference should validate: %v", err)
	}
}

// The whole reason References exists: refused at admission, with the name,
// instead of an empty string eight minutes in that looks like the earlier
// step produced nothing.
func TestLateReferenceForwardOrUnknownIsRefused(t *testing.T) {
	cases := map[string]*Registry{
		"forward": lateJob(
			Step{Run: `echo "$X"`, Late: map[string]LateValue{
				"X": fakeLate{src: "steps.later.outputs.v", refs: []string{"later"}},
			}},
			step("later", "make"),
		),
		"unknown": lateJob(
			step("build", "make"),
			Step{Run: `echo "$X"`, Late: map[string]LateValue{
				"X": fakeLate{src: "steps.nope.outputs.v", refs: []string{"nope"}},
			}},
		),
		"in a condition": lateJob(
			step("build", "make"),
			Step{Run: "make", If: fakeLate{src: "steps.nope.outcome == 'success'", refs: []string{"nope"}}},
		),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			err := r.Validate()
			if err == nil {
				t.Fatal("should be refused")
			}
			if !strings.Contains(err.Error(), "not an earlier step") {
				t.Errorf("unhelpful: %v", err)
			}
		})
	}
}

// A late binding is an env value and gets no exemption for arriving later.
func TestLateKeysObeyEnvRules(t *testing.T) {
	cases := map[string]Step{
		"reserved prefix": {Run: "make", Late: map[string]LateValue{
			"LATEM_BUILD_NUMBER": fakeLate{src: "job.status"},
		}},
		"not an identifier": {Run: "make", Late: map[string]LateValue{
			"not-a-key": fakeLate{src: "job.status"},
		}},
		"collides with static env": {Run: "make",
			Env:  map[string]string{"X": "static"},
			Late: map[string]LateValue{"X": fakeLate{src: "job.status"}},
		},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if err := lateJob(s).Validate(); err == nil {
				t.Error("should be refused")
			}
		})
	}
}

func TestDuplicateStepIDIsRefused(t *testing.T) {
	r := lateJob(step("x", "make a"), step("x", "make b"))
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("duplicate id: %v", err)
	}
}

// The native format cannot write any of this. A key that looks like it is
// trying is an unknown field, which strict decoding refuses.
func TestNativeFormatCannotWriteLateFields(t *testing.T) {
	for _, body := range []string{
		"version: 1\njobs:\n  j:\n    runs_on: [macos]\n    steps:\n      - run: make\n        id: x\n",
		"version: 1\njobs:\n  j:\n    runs_on: [macos]\n    steps:\n      - run: make\n        late: {}\n",
		"version: 1\njobs:\n  j:\n    runs_on: [macos]\n    steps:\n      - run: make\n        if: always()\n",
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("jobs.yaml should not be able to write late fields:\n%s", body)
		}
	}
}

// Admission must carry the bindings through, or the run loop has nothing to
// resolve and the step silently runs with an empty variable.
func TestAdmitCarriesLateBindings(t *testing.T) {
	late := fakeLate{src: "steps.build.outputs.sha", refs: []string{"build"}, val: "abc"}
	cond := fakeLate{src: "always()"}
	r := lateJob(
		step("build", "make"),
		Step{Name: "use", Run: `echo "$SHA"`, Late: map[string]LateValue{"SHA": late}, If: cond},
	)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	res, dec := r.Admit(protocol.RunRequest{Job: "j", Ref: "refs/heads/main"}, []string{"macos"})
	if dec.Code != "" {
		t.Fatalf("rejected: %+v", dec)
	}
	got := res.Steps[1]
	if got.ID != "" || res.Steps[0].ID != "build" {
		t.Errorf("ids = %q, %q", res.Steps[0].ID, got.ID)
	}
	if got.Late["SHA"] == nil || got.Late["SHA"].Source() != late.src {
		t.Errorf("late binding not carried: %+v", got.Late)
	}
	if got.If == nil || got.If.Source() != "always()" {
		t.Errorf("condition not carried: %+v", got.If)
	}
	// And the static env is untouched by the late one.
	if _, present := got.Env["SHA"]; present {
		t.Error("a late value must not be pre-filled into the static env")
	}
}
