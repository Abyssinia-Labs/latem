//go:build unix

package native

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

// requireHonestLoginShell skips when this machine's login profile rewrites
// LANG.
//
// It is the hole locale.go documents, met in practice: a step runs under
// `bash -lc`, the profile runs *after* execve, and an `export LANG=...` in
// /etc/profile.d or ~/.bash_profile beats anything the executor handed over.
// Both GitHub-hosted runners do exactly this, which is how three tests that
// pass on a developer's laptop failed on every CI run for a month.
//
// The behaviour is correct and documented; what was wrong was asserting the
// end-to-end value on a machine that is entitled to override it. So the
// override tests below observe the real shell where that is meaningful, skip
// where it is not, and the layering itself is pinned unconditionally by
// [TestLocale_TheLayeringReachesExecve] — which reads the environment handed to
// execve and no profile can touch.
func requireHonestLoginShell(t *testing.T) {
	t.Helper()
	requireShell(t)

	// The real environment with the sentinel overlaid, which is what the
	// executor does — and it matters that HOME is the real one, because
	// ~/.bash_profile is as entitled to rewrite LANG as /etc/profile is. A
	// probe run against an empty HOME would measure a different machine from
	// the one the tests below run on.
	const sentinel = "zz_LATEM_PROBE"
	cmd := exec.Command(DefaultShell, "-lc", `printf %s "${LANG-}"`) //nolint:gosec // a constant
	cmd.Env = append(os.Environ(), "LANG="+sentinel)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("could not probe the login shell: %v", err)
	}
	if strings.TrimSpace(string(out)) != sentinel {
		t.Skipf("this machine's login profile rewrites LANG (a %q went in and %q came out), "+
			"so the value a step observes is the profile's rather than the executor's. That is the "+
			"hole locale.go records and `latem doctor`'s locale check reports; the layering is "+
			"asserted by TestLocale_TheLayeringReachesExecve instead.",
			sentinel, strings.TrimSpace(string(out)))
	}
}

// The defect A33 records is that a step got no locale at all. The assertion is
// therefore on the step's own view of its environment, through the shell that
// actually runs it, rather than on the map this package builds — a base layer
// that was assembled and then dropped by [buildEnviron] would satisfy the
// second and not the first.
func TestLocale_AStepGetsAUTF8LocaleWithoutAskingForOne(t *testing.T) {
	requireHonestLoginShell(t)
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
	requireHonestLoginShell(t)
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
	requireHonestLoginShell(t)
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

// The layering, asserted where no machine can interfere: the environment handed
// to execve.
//
// This is the assertion the three tests above were reaching for, and the reason
// it is worth having both is the one stated in this file's first comment — a
// base layer assembled and then dropped by [buildEnviron] would satisfy a test
// on [mergeEnv] alone. So this goes through both, in the order [runEnv.Exec]
// uses them, and checks the value that would be passed to the shell.
func TestLocale_TheLayeringReachesExecve(t *testing.T) {
	lang := func(environ []string) string {
		for _, kv := range environ {
			if name, value, ok := strings.Cut(kv, "="); ok && name == "LANG" {
				return value
			}
		}
		return "absent"
	}
	// An inherited locale, because a machine usually has one and the question
	// is whether it can shadow the layers above it.
	inherited := []string{"LANG=C.UTF-8", "PATH=/usr/bin:/bin"}

	for _, tc := range []struct {
		name                string
		spec, carried, step map[string]string
		want                string
	}{
		{
			name: "nothing asks, so the default is handed over",
			want: DefaultLang,
		},
		{
			name: "the run environment wins over the default",
			spec: map[string]string{"LANG": "en_GB.UTF-8"},
			want: "en_GB.UTF-8",
		},
		{
			name: "the step wins over the run",
			spec: map[string]string{"LANG": "en_GB.UTF-8"},
			step: map[string]string{"LANG": "C"},
			want: "C",
		},
		{
			// $LATEM_ENV between steps (§9). It is above the base layer for
			// the same reason the run is: a job that exported a locale in one
			// step meant it for the next.
			name:    "a carried value wins over the default",
			carried: map[string]string{"LANG": "ja_JP.UTF-8"},
			want:    "ja_JP.UTF-8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeEnv(baseEnv(), tc.spec, tc.carried, tc.step)
			if got := lang(buildEnviron(inherited, merged)); got != tc.want {
				t.Errorf("LANG handed to execve = %q, want %q", got, tc.want)
			}
		})
	}
}
