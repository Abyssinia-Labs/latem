package docker

import (
	"strings"
	"testing"
)

func TestParseProbeOutput(t *testing.T) {
	out := strings.Join([]string{
		"passwd_name=runner",
		"home=/home/runner",
		"home_exists=1",
		"",
		"some unrelated line the image printed",
		"tool_bash=1",
		"empty=",
	}, "\n")

	got := parseProbeOutput(out)
	want := map[string]string{
		"passwd_name": "runner",
		"home":        "/home/runner",
		"home_exists": "1",
		"tool_bash":   "1",
		"empty":       "",
	}
	assertEnv(t, got, want)
}

// A conforming image, so the happy path is pinned as well as the failures.
func TestBuildImageReport_ConformingImage(t *testing.T) {
	r := buildImageReport("ci:1", map[string]string{
		"passwd_name":   "runner",
		"group_name":    "runner",
		"home":          "/home/runner",
		"home_exists":   "1",
		"home_writable": "1",
		"tool_bash":     "1",
		"tool_git":      "1",
		"ca":            "1",
	})
	if !r.OK() {
		t.Errorf("a conforming image failed: %v", r.Failures())
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

// The specific failure §13.1 warns about: no uid 1000 in /etc/passwd, so
// $HOME is unset or unwritable, and the symptom shows up forty minutes later
// as an npm or Gradle error that looks like a tooling bug.
func TestBuildImageReport_NoUID1000(t *testing.T) {
	r := buildImageReport("ci:1", map[string]string{
		"passwd_name":   "",
		"group_name":    "",
		"home":          "/",
		"home_exists":   "1",
		"home_writable": "0",
		"tool_bash":     "1",
		"tool_git":      "1",
		"ca":            "1",
	})
	if r.OK() {
		t.Fatal("an image with no uid 1000 passed")
	}
	failed := map[string]bool{}
	for _, c := range r.Failures() {
		failed[c.Name] = true
	}
	for _, want := range []string{"user-1000", "group-1000", "home-writable"} {
		if !failed[want] {
			t.Errorf("check %q passed; it should not have", want)
		}
	}
	err := r.Err()
	if err == nil {
		t.Fatal("Err returned nil for a failing report")
	}
	if !strings.Contains(err.Error(), "ci:1") {
		t.Errorf("error %q does not name the image", err)
	}
}

func TestBuildImageReport_Checks(t *testing.T) {
	conforming := map[string]string{
		"passwd_name": "runner", "group_name": "runner",
		"home": "/home/runner", "home_exists": "1", "home_writable": "1",
		"tool_bash": "1", "tool_git": "1", "ca": "1",
	}

	tests := map[string]struct {
		mutate    map[string]string
		wantFails []string
	}{
		"wrong name for uid 1000": {map[string]string{"passwd_name": "builder"}, []string{"user-1000"}},
		"missing bash":            {map[string]string{"tool_bash": "0"}, []string{"tool-bash"}},
		"missing git":             {map[string]string{"tool_git": "0"}, []string{"tool-git"}},
		"missing ca bundle":       {map[string]string{"ca": "0"}, []string{"ca-certificates"}},
		"home does not exist":     {map[string]string{"home_exists": "0"}, []string{"home-exists"}},
		"home unset":              {map[string]string{"home": ""}, []string{"home-exists"}},
		"nothing at all":          {map[string]string{}, nil},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fields := map[string]string{}
			for k, v := range conforming {
				fields[k] = v
			}
			for k, v := range tc.mutate {
				fields[k] = v
			}
			r := buildImageReport("ci:1", fields)

			failed := map[string]bool{}
			for _, c := range r.Failures() {
				failed[c.Name] = true
			}
			for _, want := range tc.wantFails {
				if !failed[want] {
					t.Errorf("check %q passed, want it to fail", want)
				}
			}
			if len(tc.wantFails) > 0 && len(r.Failures()) != len(tc.wantFails) {
				t.Errorf("failures = %v, want exactly %v", r.Failures(), tc.wantFails)
			}
		})
	}
}

// An empty fact map is what a probe that could not run at all produces. Every
// check must fail rather than defaulting to "fine".
func TestBuildImageReport_EmptyFactsFailEverything(t *testing.T) {
	r := buildImageReport("ci:1", map[string]string{})
	if r.OK() {
		t.Fatal("an image that produced no output passed the contract")
	}
	if len(r.Failures()) != len(r.Checks) {
		t.Errorf("%d of %d checks passed on no evidence", len(r.Checks)-len(r.Failures()), len(r.Checks))
	}
}

// The detail is what an operator acts on, so it has to say what to do rather
// than only what is wrong.
func TestImageCheckDetailsAreActionable(t *testing.T) {
	r := buildImageReport("ci:1", map[string]string{})
	for _, c := range r.Checks {
		if c.Detail == "" {
			t.Errorf("check %q has no detail", c.Name)
		}
	}
}

// The probe runs under /bin/sh because whether bash exists is one of the
// things it reports. A bashism here would make every non-bash image look
// broken in the same way.
func TestImageProbeScriptIsPortable(t *testing.T) {
	for _, bashism := range []string{"[[", "$'", "declare ", "local ", "function "} {
		if strings.Contains(imageProbeScript, bashism) {
			t.Errorf("the probe script uses %q, which /bin/sh may not have", bashism)
		}
	}
	for _, want := range []string{"/etc/passwd", "/etc/group", "for t in bash git", `tool_$t`} {
		if !strings.Contains(imageProbeScript, want) {
			t.Errorf("the probe script never looks at %q", want)
		}
	}
}
