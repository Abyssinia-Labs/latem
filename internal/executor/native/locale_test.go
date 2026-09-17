//go:build unix

package native

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

// The defect A33 records is that a step got no locale at all. The assertion is
// therefore on the step's own view of its environment, through the shell that
// actually runs it, rather than on the map this package builds — a base layer
// that was assembled and then dropped by [buildEnviron] would satisfy the
// second and not the first.
func TestLocale_AStepGetsAUTF8LocaleWithoutAskingForOne(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})

	_, stdout, _ := run(t, env, executor.Step{
		Name: "report the locale",
		Run:  `printf 'LANG=%s\n' "${LANG-unset}"`,
	})

	if got := strings.TrimSpace(stdout); got != "LANG="+DefaultLang {
		t.Fatalf("a step's LANG = %q, want %q", got, "LANG="+DefaultLang)
	}
}

// Both directions of the override, because a default that cannot be overridden
// is not a default. A job building software whose tests assert on C collation
// has to be able to say so.
func TestLocale_TheJobWins(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})

	_, stdout, _ := run(t, env, executor.Step{
		Name: "a step that chooses its own locale",
		Env:  map[string]string{"LANG": "C"},
		Run:  `printf '%s\n' "$LANG"`,
	})
	if got := strings.TrimSpace(stdout); got != "C" {
		t.Fatalf("steps[].env did not override the default: LANG = %q", got)
	}
}

func TestLocale_TheRunEnvironmentWins(t *testing.T) {
	env, _ := live(t, executor.Spec{Env: map[string]string{"LANG": "en_GB.UTF-8"}}, Options{})

	_, stdout, _ := run(t, env, executor.Step{
		Name: "report the locale",
		Run:  `printf '%s\n' "$LANG"`,
	})
	if got := strings.TrimSpace(stdout); got != "en_GB.UTF-8" {
		t.Fatalf("the run environment did not override the default: LANG = %q", got)
	}
}

// The layer sits below the run environment, so it must not be able to introduce
// a variable the agent did not mean to hand over. One key, and the test says so
// rather than assuming a future reader will notice a second appearing.
func TestLocale_TheBaseLayerIsOnlyTheLocale(t *testing.T) {
	base := baseEnv()
	if len(base) != 1 {
		t.Fatalf("baseEnv has %d entries, want 1: %v", len(base), base)
	}
	if base["LANG"] != DefaultLang {
		t.Fatalf("baseEnv[LANG] = %q, want %q", base["LANG"], DefaultLang)
	}
}

// A map returned to a caller that mutates it must not change what the next run
// gets. This is why baseEnv is a function.
func TestLocale_TheBaseLayerIsNotShared(t *testing.T) {
	first := baseEnv()
	first["LANG"] = "C"
	first["INJECTED"] = "1"

	second := baseEnv()
	if second["LANG"] != DefaultLang {
		t.Fatalf("mutating one baseEnv changed the next: LANG = %q", second["LANG"])
	}
	if _, ok := second["INJECTED"]; ok {
		t.Fatal("mutating one baseEnv added a key to the next")
	}
}
