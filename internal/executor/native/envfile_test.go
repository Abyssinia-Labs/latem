package native

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEnvFile_Grammar(t *testing.T) {
	tests := map[string]struct {
		in       string
		want     map[string]string
		warnings int
	}{
		"one assignment": {
			in:   "BUILD_NUMBER=42\n",
			want: map[string]string{"BUILD_NUMBER": "42"},
		},
		"blank lines are not lines": {
			in:   "\n\nA=1\n\n  \n",
			want: map[string]string{"A": "1"},
		},
		"crlf": {
			in:   "A=1\r\nB=2\r\n",
			want: map[string]string{"A": "1", "B": "2"},
		},
		"no trailing newline": {
			in:   "A=1",
			want: map[string]string{"A": "1"},
		},
		"value keeps everything after the first equals": {
			in:   "URL=https://x/y?a=b&c=d\n",
			want: map[string]string{"URL": "https://x/y?a=b&c=d"},
		},
		"empty value is legal": {
			in:   "A=\n",
			want: map[string]string{"A": ""},
		},
		"value is never interpreted": {
			in:   "PATH_LIKE=x; rm -rf /\n",
			want: map[string]string{"PATH_LIKE": "x; rm -rf /"},
		},
		"underscore start": {
			in:   "_PRIVATE=1\n",
			want: map[string]string{"_PRIVATE": "1"},
		},
		"no equals is dropped": {
			in:       "just some log output\n",
			want:     map[string]string{},
			warnings: 1,
		},
		"key with a space is dropped": {
			in:       "NOT AN IDENT=1\n",
			want:     map[string]string{},
			warnings: 1,
		},
		"key starting with a digit is dropped": {
			in:       "1ABC=1\n",
			want:     map[string]string{},
			warnings: 1,
		},
		"key with a hyphen is dropped": {
			in:       "A-B=1\n",
			want:     map[string]string{},
			warnings: 1,
		},
		"empty key is dropped": {
			in:       "=value\n",
			want:     map[string]string{},
			warnings: 1,
		},
		"last assignment to a key wins": {
			in:   "A=1\nA=2\n",
			want: map[string]string{"A": "2"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, warnings := parseEnvFile([]byte(tc.in), nil)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("vars = %v, want %v", got, tc.want)
			}
			if len(warnings) != tc.warnings {
				t.Errorf("got %d warnings %q, want %d", len(warnings), warnings, tc.warnings)
			}
		})
	}
}

// §9: a job may not overwrite a declared secret or spoof a LATEM_* variable.
// The whole point of the file is that the agent trusts what a step wrote into
// it, so the keys it may not write are the entire security property.
func TestParseEnvFile_ProtectedKeys(t *testing.T) {
	runEnvironment := map[string]string{
		"MATCH_PASSWORD": "hunter2",
		"NODE_ENV":       "production",
	}

	t.Run("secrets named, ordinary variables may be overridden", func(t *testing.T) {
		protected := func(k string) bool {
			return protectedEnvKey(runEnvironment, []string{"MATCH_PASSWORD"}, k)
		}
		got, warnings := parseEnvFile([]byte("MATCH_PASSWORD=stolen\nNODE_ENV=test\nOK=1\n"), protected)

		want := map[string]string{"NODE_ENV": "test", "OK": "1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("vars = %v, want %v", got, want)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "MATCH_PASSWORD") {
			t.Errorf("warnings = %q, want one naming MATCH_PASSWORD", warnings)
		}
	})

	// Amendment A10's fallback. An executor with no Secrets list cannot tell a
	// credential from NODE_ENV, so it refuses every injected key. Failing open
	// would be a credential overwrite; this is the cheaper mistake.
	t.Run("no secrets list protects the whole injected environment", func(t *testing.T) {
		protected := func(k string) bool { return protectedEnvKey(runEnvironment, nil, k) }
		got, warnings := parseEnvFile([]byte("MATCH_PASSWORD=stolen\nNODE_ENV=test\nOK=1\n"), protected)

		if !reflect.DeepEqual(got, map[string]string{"OK": "1"}) {
			t.Errorf("vars = %v, want only OK", got)
		}
		if len(warnings) != 2 {
			t.Errorf("warnings = %q, want two", warnings)
		}
	})

	t.Run("every LATEM_ name is refused", func(t *testing.T) {
		for _, key := range []string{"LATEM_ENV", "LATEM_RUN_ID", "LATEM_SECRET_X", "LATEM_"} {
			if !protectedEnvKey(nil, []string{"OTHER"}, key) {
				t.Errorf("%s is not protected; a job could spoof the agent's own variables", key)
			}
		}
		if protectedEnvKey(nil, []string{"OTHER"}, "LATEMISH") {
			t.Error("LATEMISH was refused; the namespace is LATEM_, not LATEM")
		}
	})
}

// The cap is what stops `yes >> "$LATEM_ENV"` from filling the agent's memory.
// On this executor there is no container quota underneath it, only the host's
// own disk, so the read-side limit is the only limit there is.
func TestParseEnvFile_SizeCap(t *testing.T) {
	t.Run("under the cap is silent", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(strings.Repeat("A=1\n", 100))
		_, warnings := parseEnvFile([]byte(b.String()), nil)
		if len(warnings) != 0 {
			t.Errorf("warnings = %q, want none", warnings)
		}
	})

	t.Run("over the cap drops the tail and the partial line at the cut", func(t *testing.T) {
		var b strings.Builder
		for b.Len() < MaxEnvFileBytes {
			b.WriteString("KEPT=x\n")
		}
		b.WriteString("DROPPED=y\n")

		got, warnings := parseEnvFile([]byte(b.String()), nil)
		if _, ok := got["DROPPED"]; ok {
			t.Error("a key past the 64 KB cap was kept")
		}
		if got["KEPT"] != "x" {
			t.Error("everything before the cap should survive")
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "limit") {
			t.Errorf("warnings = %q, want one naming the limit", warnings)
		}
	})

	t.Run("over the cap with no newline at all yields nothing", func(t *testing.T) {
		got, warnings := parseEnvFile([]byte("A="+strings.Repeat("x", MaxEnvFileBytes)), nil)
		if len(got) != 0 {
			t.Errorf("vars = %v, want none: half a value must never become a whole one", got)
		}
		if len(warnings) != 1 {
			t.Errorf("warnings = %q, want one", warnings)
		}
	})
}

// A malformed line may be anything, including a credential a script printed
// into the wrong file descriptor. Explaining that it was dropped by quoting it
// would be its own leak.
func TestParseEnvFile_WarningsNeverEchoTheLine(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	_, warnings := parseEnvFile([]byte(
		"this line has no equals "+secret+"\n"+
			"bad key="+secret+"\n"), nil)

	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want two", warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w, secret) {
			t.Errorf("warning %q echoes the dropped content", w)
		}
	}
}

// A protected key is a well-formed identifier by the time it is refused, so
// naming it is safe and is the only way the author learns which key was
// ignored. The *value* still never appears.
func TestParseEnvFile_ProtectedWarningNamesTheKeyNotTheValue(t *testing.T) {
	protected := func(k string) bool { return k == "MATCH_PASSWORD" }
	_, warnings := parseEnvFile([]byte("MATCH_PASSWORD=hunter2\n"), protected)

	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want one", warnings)
	}
	if !strings.Contains(warnings[0], "MATCH_PASSWORD") {
		t.Errorf("warning %q does not name the key", warnings[0])
	}
	if strings.Contains(warnings[0], "hunter2") {
		t.Errorf("warning %q echoes the value", warnings[0])
	}
}
