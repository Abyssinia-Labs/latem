package logstream

import (
	"encoding/base64"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A realistically shaped App Store Connect key. RUNNER_SPEC §9 declares
// APP_STORE_CONNECT_KEY_P8 as a secret in its own example job, so this is not
// a hypothetical input.
const pemFixture = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj
MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu
NMoSfm76oqFvAp8Gy0iz5sxjZmSnXyCdPEovGhLa0VzMaQ8s+CLOyS56YyCFGeJZ
qgtzJ6GR3eqoYSW9b9UMvkBpZODSctWSNGj3P7jRFDO5VoTwCQAWbFnOjDfH5Ulg
-----END PRIVATE KEY-----`

// This is the test finding F3 exists for.
//
// The pipeline splits into lines and then redacts each one, so registering
// only the whole value of a multi-line secret redacts nothing at all — the
// whole value never appears in any single line. A version of this test that
// echoed the secret as one string would pass against that broken
// implementation, which is exactly how draft 01 of the spec shipped an
// unredacted App Store Connect key with a green suite.
//
// So: register once, then assert line by line.
func TestRedact_MultiLineSecret_EveryLineIsRedacted(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte(pemFixture))

	for i, line := range strings.Split(pemFixture, "\n") {
		got := r.Redact(line)
		if strings.Contains(got, strings.TrimSpace(line)) && len(strings.TrimSpace(line)) >= MinRedactableLength {
			t.Errorf("line %d survived redaction:\n  in:  %s\n  out: %s", i, line, got)
		}
		if !strings.Contains(got, Marker) {
			t.Errorf("line %d produced no redaction marker: %q", i, got)
		}
	}
}

// The body lines are the part that matters. A PEM's delimiters are matched by
// the always-on patterns, so an implementation that only redacts those looks
// like it works while leaking the entire key.
func TestRedact_MultiLineSecret_BodyLinesSpecifically(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte(pemFixture))

	lines := strings.Split(pemFixture, "\n")
	body := lines[1 : len(lines)-1] // strip BEGIN and END

	for _, line := range body {
		got := r.Redact(line)
		if strings.Contains(got, line) {
			t.Fatalf("PEM body line emitted in the clear:\n  %s", got)
		}
		// And a substring of it, in case something redacted only a prefix.
		if strings.Contains(got, line[10:40]) {
			t.Fatalf("part of a PEM body line survived:\n  %s", got)
		}
	}
}

// Tooling re-emits PEMs as one long line often enough that rule 3 is not
// hypothetical.
func TestRedact_WhitespaceStrippedForm(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte(pemFixture))

	oneLine := strings.Join(strings.Fields(pemFixture), "")
	if got := r.Redact(oneLine); strings.Contains(got, "MIIEvQIBADANBgkqhkiG") {
		t.Errorf("whitespace-stripped form survived: %s", got)
	}
}

// A base64'd token in a log is still a leaked token.
func TestRedact_EncodedForms(t *testing.T) {
	const secret = "super-secret-value-1234"
	r := NewRedactor()
	r.Register([]byte(secret))

	for name, encoded := range map[string]string{
		"std base64":     base64.StdEncoding.EncodeToString([]byte(secret)),
		"raw base64":     base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"url base64":     base64.URLEncoding.EncodeToString([]byte(secret)),
		"raw url base64": base64.RawURLEncoding.EncodeToString([]byte(secret)),
		"query escaped":  url.QueryEscape(secret),
		"path escaped":   url.PathEscape(secret),
	} {
		t.Run(name, func(t *testing.T) {
			line := "token=" + encoded
			if got := r.Redact(line); strings.Contains(got, encoded) {
				t.Errorf("%s form survived: %s", name, got)
			}
		})
	}
}

func TestRedact_PlainValue(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte("hunter2-and-then-some"))

	got := r.Redact("password is hunter2-and-then-some ok")
	if strings.Contains(got, "hunter2") {
		t.Errorf("secret survived: %s", got)
	}
	if !strings.HasPrefix(got, "password is ***") {
		t.Errorf("unexpected shape: %s", got)
	}
	if !strings.HasSuffix(got, " ok") {
		t.Errorf("surrounding text was damaged: %s", got)
	}
}

// Matching values this short turns every "true" and "dev" into a marker and
// makes logs unreadable. The floor is documented, and TooShort exists so
// callers can warn the operator by name.
func TestRedact_ShortValuesAreNotRedacted(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte("dev"))

	if got := r.Redact("environment is dev"); got != "environment is dev" {
		t.Errorf("short value was redacted: %s", got)
	}
	if !TooShort([]byte("dev")) {
		t.Error("TooShort should flag a 3-character secret")
	}
	if TooShort([]byte("long-enough")) {
		t.Error("TooShort should not flag a long secret")
	}
}

// Overlapping matches are merged rather than resolved leftmost-longest. With
// leftmost-longest, the first match truncates the second and the tail of the
// second secret survives — this test pins that it does not.
func TestRedact_OverlappingMatchesAreUnioned(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte("SECRETVALUE"))
	r.Register([]byte("VALUEEXTRA"))

	got := r.Redact("xSECRETVALUEEXTRAy")
	if strings.Contains(got, "EXTRA") {
		t.Errorf("tail of an overlapping secret survived: %s", got)
	}
	if !strings.HasPrefix(got, "x") || !strings.HasSuffix(got, "y") {
		t.Errorf("surrounding text was damaged: %s", got)
	}
}

func TestRedact_AlwaysOnPatterns(t *testing.T) {
	r := NewRedactor() // nothing registered

	for name, line := range map[string]string{
		"aws key":      "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"github token": "token ghp_abcdefghijklmnopqrstuvwxyz0123456789AB",
		"slack token":  "hook xoxb-123456789012-abcdefghijkl",
		"jwt":          "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0",
		"pem header":   "-----BEGIN RSA PRIVATE KEY-----",
	} {
		t.Run(name, func(t *testing.T) {
			got := r.Redact(line)
			if got == line {
				t.Errorf("pattern not matched: %s", got)
			}
			if !strings.Contains(got, Marker) {
				t.Errorf("no marker: %s", got)
			}
		})
	}
}

// A pattern that redacts only the recognisable prefix of a credential is worse
// than no pattern, because it looks like it worked.
func TestRedact_PatternsCoverTheWholeCredential(t *testing.T) {
	r := NewRedactor()

	if got := r.Redact("xoxb-123456789012-abcdefghijkl"); strings.Contains(got, "abcdefghijkl") {
		t.Errorf("slack token body survived: %s", got)
	}
	if got := r.Redact("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9"); strings.Contains(got, "eyJzdWIi") {
		t.Errorf("jwt payload survived: %s", got)
	}
}

// The tag lets an operator correlate occurrences of one secret across a log
// without learning it.
func TestRedact_FingerprintIsStableAndDistinct(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte("first-secret-value"))
	r.Register([]byte("second-secret-value"))

	a1 := r.Redact("x first-secret-value")
	a2 := r.Redact("y first-secret-value")
	b := r.Redact("z second-secret-value")

	tag := func(s string) string {
		i := strings.Index(s, Marker)
		if i < 0 {
			t.Fatalf("no redaction marker in %q", s)
		}
		return s[i:]
	}

	if tag(a1) != tag(a2) {
		t.Errorf("same secret produced different tags: %q vs %q", tag(a1), tag(a2))
	}
	if tag(a1) == tag(b) {
		t.Error("different secrets produced the same tag")
	}
	if len(tag(a1)) != len(Marker)+6 {
		t.Errorf("tag is %q, want a six-character suffix", tag(a1))
	}
}

func TestRedact_LeavesOrdinaryOutputAlone(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte("some-real-secret"))

	for _, line := range []string{
		"",
		"> Task :app:compileKotlin",
		"BUILD SUCCESSFUL in 1m 42s",
		"warning: unused variable 'x' at src/main.go:14",
		"Downloading https://example.com/artifact-1.2.3.jar",
	} {
		if got := r.Redact(line); got != line {
			t.Errorf("ordinary output was modified:\n  in:  %q\n  out: %q", line, got)
		}
	}
}

// Registration happens before the first step, redaction on every line after —
// but a provider chain resolving secrets concurrently must not race the
// pipeline. Run under -race.
func TestRedact_ConcurrentRegisterAndRedact(t *testing.T) {
	r := NewRedactor()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Register([]byte(strings.Repeat("secret", i+2)))
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Redact("some line with secretsecret in it")
			}
		}()
	}
	wg.Wait()
}

// Registering the same value twice must not rebuild or duplicate anything, and
// must not change the output.
func TestRedact_IdempotentRegistration(t *testing.T) {
	r := NewRedactor()
	r.Register([]byte("repeated-secret-value"))
	first := r.Redact("x repeated-secret-value")
	r.Register([]byte("repeated-secret-value"))
	if second := r.Redact("x repeated-secret-value"); second != first {
		t.Errorf("re-registration changed output: %q → %q", first, second)
	}
}

func TestExpandForms_MultiLineProducesPerLineEntries(t *testing.T) {
	forms := expandForms([]byte(pemFixture))

	lines := strings.Split(pemFixture, "\n")
	for _, line := range lines[1 : len(lines)-1] {
		found := false
		for _, f := range forms {
			if f == line {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no per-line form registered for %q", line[:20]+"...")
		}
	}
}
