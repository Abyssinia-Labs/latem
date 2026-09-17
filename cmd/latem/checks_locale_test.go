package main

import (
	"strings"
	"testing"
)

func TestDoctorLocale(t *testing.T) {
	cases := map[string]struct {
		answer     fakeCmd
		want       level
		wantDetail string
	}{
		"a UTF-8 locale reaches the step": {
			answer: localeAnswer("en_US.UTF-8", "", "UTF-8"),
			want:   levelPass,
		},
		// The hole A33's implementation deliberately leaves: a login shell's
		// profile runs after execve, so LC_ALL there beats the LANG the
		// executor set. The check exists to name that rather than hide it.
		"a profile setting LC_ALL wins": {
			answer:     localeAnswer("en_US.UTF-8", "C", "US-ASCII"),
			want:       levelFail,
			wantDetail: "LC_ALL=C",
		},
		"no UTF-8 at all": {
			answer:     localeAnswer("", "", "US-ASCII"),
			want:       levelFail,
			wantDetail: "US-ASCII",
		},
		// An LC_ALL that is itself UTF-8 is not a fault, even though it is set.
		"a UTF-8 LC_ALL is fine": {
			answer: localeAnswer("en_US.UTF-8", "en_GB.UTF-8", "UTF-8"),
			want:   levelPass,
		},
		"a shell that cannot be probed is a warning, not a failure": {
			answer: fakeCmd{exit: 127},
			want:   levelWarn,
		},
		// Output that is not three fields must not panic or read a field that
		// is not there.
		"a truncated answer": {
			answer: fakeCmd{out: "en_US.UTF-8\n"},
			want:   levelFail,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := privateDir(t)
			cmds := healthyRunner("darwin")
			cmds[localeProbe] = tc.answer
			d := newTestDoctor("darwin", dir, cmds, testConfig(dir), nil)

			got := d.checkLocale(t.Context())
			if got.Level != tc.want {
				t.Fatalf("level = %v, want %v (detail %q)", got.Level, tc.want, got.Detail)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("detail %q does not mention %q", got.Detail, tc.wantDetail)
			}
			if got.Level == levelFail && len(got.Fix) == 0 {
				t.Error("a failing locale check carries no fix")
			}
		})
	}
}
