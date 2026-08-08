package docker

import (
	"strings"
	"testing"
)

// noneProtected is the predicate for tests that are about grammar, not policy.
func noneProtected(string) bool { return false }

func TestParseEnvFile(t *testing.T) {
	tests := map[string]struct {
		in   string
		want map[string]string
	}{
		"empty":            {"", map[string]string{}},
		"single":           {"A=1\n", map[string]string{"A": "1"}},
		"no trailing lf":   {"A=1", map[string]string{"A": "1"}},
		"multiple":         {"A=1\nB=2\n", map[string]string{"A": "1", "B": "2"}},
		"crlf":             {"A=1\r\nB=2\r\n", map[string]string{"A": "1", "B": "2"}},
		"blank lines":      {"A=1\n\n\nB=2\n", map[string]string{"A": "1", "B": "2"}},
		"empty value":      {"A=\n", map[string]string{"A": ""}},
		"underscore start": {"_A=1\n", map[string]string{"_A": "1"}},
		"digits in key":    {"A1_B2=x\n", map[string]string{"A1_B2": "x"}},

		// The value is taken literally after the first "=". A job writing a
		// connection string or a base64 blob must not have it reinterpreted.
		"value with equals": {"A=b=c=d\n", map[string]string{"A": "b=c=d"}},
		"value with spaces": {"A=hello world\n", map[string]string{"A": "hello world"}},
		"value with quotes": {`A="quoted"` + "\n", map[string]string{"A": `"quoted"`}},
		"value with shell":  {"A=$(rm -rf /)\n", map[string]string{"A": "$(rm -rf /)"}},

		// Last write wins, which is what a shell would do with two exports.
		"duplicate key": {"A=1\nA=2\n", map[string]string{"A": "2"}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, warnings := parseEnvFile([]byte(tc.in), noneProtected)
			assertEnv(t, got, tc.want)
			if len(warnings) != 0 {
				t.Errorf("unexpected warnings: %q", warnings)
			}
		})
	}
}

// A key that is not an identifier is a key a shell could not have exported
// either. Dropping it silently would leave a job author staring at a variable
// that is simply not there.
func TestParseEnvFile_MalformedLinesAreDroppedWithAWarning(t *testing.T) {
	tests := map[string]string{
		"no equals sign":     "just some output",
		"leading digit":      "1BAD=x",
		"hyphen in key":      "BAD-KEY=x",
		"dot in key":         "bad.key=x",
		"space in key":       "BAD KEY=x",
		"empty key":          "=x",
		"comment":            "# a comment",
		"shell fragment":     "export A=1",
		"non-ascii key":      "Ké=x",
		"whitespace padding": " A=1",
	}

	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			got, warnings := parseEnvFile([]byte(line+"\n"), noneProtected)
			if len(got) != 0 {
				t.Errorf("accepted %q as %v, want it dropped", line, got)
			}
			if len(warnings) != 1 {
				t.Fatalf("got %d warnings %q, want exactly 1", len(warnings), warnings)
			}
			// The warning must not echo the line: a malformed line may be
			// anything, including a credential a script printed by mistake.
			if strings.Contains(warnings[0], line) {
				t.Errorf("warning %q quotes the dropped line; it must not", warnings[0])
			}
		})
	}
}

func TestParseEnvFile_ProtectedKeysAreIgnored(t *testing.T) {
	runEnvironment := map[string]string{
		"MATCH_PASSWORD": "real-secret",
		"NPM_TOKEN":      "real-token",
		"LATEM_RUN_ID":   "r1",
	}
	protected := func(k string) bool { return protectedEnvKey(runEnvironment, k) }

	in := strings.Join([]string{
		"MATCH_PASSWORD=stolen",
		"NPM_TOKEN=stolen",
		"LATEM_ENV=/tmp/attacker",
		"LATEM_RUN_ID=someone-elses-run",
		"LATEM_ANYTHING=x",
		"BUILD_NUMBER=42",
	}, "\n")

	got, warnings := parseEnvFile([]byte(in), protected)
	assertEnv(t, got, map[string]string{"BUILD_NUMBER": "42"})
	if len(warnings) != 5 {
		t.Fatalf("got %d warnings %q, want 5", len(warnings), warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w, "real-secret") || strings.Contains(w, "stolen") {
			t.Errorf("warning %q leaks a value", w)
		}
	}
}

func TestProtectedEnvKey(t *testing.T) {
	runEnvironment := map[string]string{"MATCH_PASSWORD": "x", "NODE_ENV": "production"}

	tests := map[string]struct {
		key  string
		want bool
	}{
		"declared secret":          {"MATCH_PASSWORD", true},
		"any injected name":        {"NODE_ENV", true},
		"latem env itself":         {"LATEM_ENV", true},
		"any latem name":           {"LATEM_FUTURE_THING", true},
		"unrelated name":           {"BUILD_NUMBER", false},
		"latem lowercase is not":   {"latem_env", false},
		"substring is not a match": {"MY_LATEM_VAR", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := protectedEnvKey(runEnvironment, tc.key); got != tc.want {
				t.Errorf("protectedEnvKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// 64 KB is the cap §9 puts on a file the agent reads from inside a container.
// Without it, a job could make the agent allocate as much as it likes.
func TestParseEnvFile_CapIsEnforced(t *testing.T) {
	var b strings.Builder
	i := 0
	for b.Len() < MaxEnvFileBytes+4096 {
		b.WriteString("K")
		b.WriteString(strings.Repeat("0", 6))
		b.WriteString(itoa(i))
		b.WriteString("=v\n")
		i++
	}

	got, warnings := parseEnvFile([]byte(b.String()), noneProtected)
	if len(got) == 0 {
		t.Fatal("everything was dropped; the cap should truncate, not discard")
	}
	if len(got) >= i {
		t.Errorf("kept %d of %d assignments; the tail past the cap should be gone", len(got), i)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "limit") {
		t.Fatalf("got warnings %q, want one naming the limit", warnings)
	}
}

// Cutting at exactly 64 KB will usually land mid-line. Half a value must not
// become a whole one — a truncated secret is still a leak, and a truncated
// version number is a wrong build.
func TestParseEnvFile_PartialLineAtTheCapIsDiscarded(t *testing.T) {
	head := strings.Repeat("A=x\n", (MaxEnvFileBytes/4)-1) // ends exactly 4 bytes short
	data := head + "TRUNCATED=abcdefghijklmnop\n"

	got, _ := parseEnvFile([]byte(data), noneProtected)
	if v, ok := got["TRUNCATED"]; ok {
		t.Errorf("kept TRUNCATED=%q from a line the cap cut in half", v)
	}
	if got["A"] != "x" {
		t.Errorf("lost the complete lines before the cap: %v", got)
	}
}

func TestParseEnvFile_ExactlyAtTheCapIsNotTruncated(t *testing.T) {
	// A long first line plus a short unterminated second one, sized so the
	// whole file is exactly MaxEnvFileBytes.
	const tail = "B=1"
	data := "A=" + strings.Repeat("x", MaxEnvFileBytes-len(tail)-3) + "\n" + tail
	if len(data) != MaxEnvFileBytes {
		t.Fatalf("test setup: data is %d bytes, want %d", len(data), MaxEnvFileBytes)
	}

	got, warnings := parseEnvFile([]byte(data), noneProtected)
	if len(warnings) != 0 {
		t.Errorf("warned at exactly the cap: %q", warnings)
	}
	if got["B"] != "1" {
		t.Errorf("dropped the last line at exactly the cap (got %d keys)", len(got))
	}
}

func assertEnv(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
