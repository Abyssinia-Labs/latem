package ghcmd

import (
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		line   string
		name   string
		value  string
		params map[string]string
	}{
		{"::add-mask::hunter2", "add-mask", "hunter2", nil},
		{"::group::Build", "group", "Build", nil},
		{"::endgroup::", "endgroup", "", nil},
		{"::debug::a message", "debug", "a message", nil},
		{"::error file=a.go,line=12::broke", "error", "broke", map[string]string{"file": "a.go", "line": "12"}},
		{"::notice title=Hi::body", "notice", "body", map[string]string{"title": "Hi"}},
		// Case-insensitive, as GitHub matches them.
		{"::Add-Mask::x", "add-mask", "x", nil},
	}
	for _, c := range cases {
		got, ok := ParseLine(c.line)
		if !ok {
			t.Errorf("%q was not recognised", c.line)
			continue
		}
		if got.Name != c.name || got.Value != c.value {
			t.Errorf("%q → name=%q value=%q", c.line, got.Name, got.Value)
		}
		for k, v := range c.params {
			if got.Param(k) != v {
				t.Errorf("%q → param %s = %q, want %q", c.line, k, got.Param(k), v)
			}
		}
	}
}

// Almost every line of a build log is not a command.
func TestOrdinaryOutputIsNotACommand(t *testing.T) {
	for _, line := range []string{
		"building...",
		"::unterminated",
		"",
		":: ::x",
	} {
		if cmd, ok := ParseLine(line); ok {
			t.Errorf("%q was read as a command: %+v", line, cmd)
		}
	}
}

// And some output is well-formed by the grammar without being a command. A
// consumer that strips command lines must use Known, or it deletes the
// compiler error the developer is trying to read.
func TestWellFormedButUnknownStaysOutput(t *testing.T) {
	for _, line := range []string{
		"::std::vector<int> is not a command",
		"::not a name!::x",
	} {
		cmd, ok := ParseLine(line)
		if !ok {
			continue // refused by the grammar, which is also fine
		}
		if cmd.Known() {
			t.Errorf("%q was treated as a real command: %+v", line, cmd)
		}
	}
	for _, name := range []string{"add-mask", "group", "endgroup", "error"} {
		cmd, ok := ParseLine("::" + name + "::x")
		if !ok || !cmd.Known() {
			t.Errorf("%s should be known", name)
		}
	}
}

// The deprecated forms are recognised so the caller can say so. Dropping one
// silently presents as an output that never arrived.
func TestDeprecatedCommandsAreRecognised(t *testing.T) {
	for _, name := range []string{"set-output", "set-env", "add-path", "save-state"} {
		cmd, ok := ParseLine("::" + name + " name=x::v")
		if !ok || !cmd.Known() || !cmd.Deprecated() {
			t.Errorf("%s should be known and deprecated: %+v", name, cmd)
		}
	}
	if cmd, _ := ParseLine("::add-mask::x"); cmd.Deprecated() {
		t.Error("add-mask is not deprecated")
	}
}

// Decoding %25 in its own pass would turn `%250A` — the escape for a literal
// "%0A" — into a newline.
func TestUnescapingIsOnePass(t *testing.T) {
	cmd, ok := ParseLine("::debug::a%250Ab")
	if !ok {
		t.Fatal("not parsed")
	}
	if cmd.Value != "a%0Ab" {
		t.Errorf("value = %q, want the literal %%0A", cmd.Value)
	}

	cmd2, _ := ParseLine("::debug::line1%0Aline2")
	if cmd2.Value != "line1\nline2" {
		t.Errorf("value = %q, want a newline", cmd2.Value)
	}
}

// A property value cannot hold `:` or `,` literally, so those are escaped too.
func TestPropertyEscapes(t *testing.T) {
	cmd, ok := ParseLine("::error title=a%3Ab%2Cc::x")
	if !ok {
		t.Fatal("not parsed")
	}
	if got := cmd.Param("title"); got != "a:b,c" {
		t.Errorf("title = %q", got)
	}
}

func TestOversizedLineIsNotACommand(t *testing.T) {
	line := "::add-mask::" + strings.Repeat("x", maxCommandBytes)
	if _, ok := ParseLine(line); ok {
		t.Error("a line past the cap should be output, not a command")
	}
}

func nothingProtected(string) bool { return false }

func TestParseKeyValueFile(t *testing.T) {
	got, warns := ParseKeyValueFile([]byte("FOO=bar\nEMPTY=\nWITH_EQ=a=b\n"), nothingProtected)
	if len(warns) != 0 {
		t.Errorf("warnings = %v", warns)
	}
	want := []Assignment{{"FOO", "bar"}, {"EMPTY", ""}, {"WITH_EQ", "a=b"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHeredoc(t *testing.T) {
	data := "KEY<<EOF\nline1\nline2\nEOF\nAFTER=yes\n"
	got, warns := ParseKeyValueFile([]byte(data), nothingProtected)
	if len(warns) != 0 {
		t.Errorf("warnings = %v", warns)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Key != "KEY" || got[0].Value != "line1\nline2" {
		t.Errorf("heredoc = %+v", got[0])
	}
	// Parsing must resume after the delimiter, not inside the value.
	if got[1] != (Assignment{"AFTER", "yes"}) {
		t.Errorf("after the heredoc = %+v", got[1])
	}
}

// Nothing is taken from a block whose end never arrived: where the value was
// meant to stop is not knowable, and "end of file" is a guess.
func TestUnterminatedHeredocSetsNothing(t *testing.T) {
	got, warns := ParseKeyValueFile([]byte("KEY<<EOF\nline1\nline2\n"), nothingProtected)
	if len(got) != 0 {
		t.Errorf("an unterminated heredoc set %+v", got)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "never closed") {
		t.Errorf("warnings = %v", warns)
	}
}

// The protection predicate is what stops a job spoofing the agent or
// overwriting an injected credential.
func TestProtectedKeysAreRefused(t *testing.T) {
	protected := func(k string) bool { return strings.HasPrefix(k, "LATEM_") || k == "NPM_TOKEN" }
	got, warns := ParseKeyValueFile(
		[]byte("LATEM_BUILD_NUMBER=9\nNPM_TOKEN=stolen\nOK=fine\nNPM_TOKEN<<E\nstolen\nE\n"),
		protected)
	if len(got) != 1 || got[0].Key != "OK" {
		t.Errorf("got %+v, want only OK", got)
	}
	joined := strings.Join(warns, " ")
	if !strings.Contains(joined, "LATEM_BUILD_NUMBER") || !strings.Contains(joined, "NPM_TOKEN") {
		t.Errorf("both refusals should be reported: %v", warns)
	}
	// And the warning must not carry what the job tried to set it to.
	if strings.Contains(joined, "stolen") {
		t.Errorf("a warning quoted the value: %v", warns)
	}
}

// The heredoc's sharp edge, bounded rather than solved: a value containing the
// delimiter ends there and the rest is read as assignments — but those still
// have to be identifiers and still have to pass protected.
func TestHeredocEarlyTerminationStaysBounded(t *testing.T) {
	protected := func(k string) bool { return k == "NPM_TOKEN" }
	data := "DESC<<EOF\nuser input\nEOF\nNPM_TOKEN=stolen\n"
	got, warns := ParseKeyValueFile([]byte(data), protected)
	for _, a := range got {
		if a.Key == "NPM_TOKEN" {
			t.Fatal("a protected key was set through an early heredoc termination")
		}
	}
	if !strings.Contains(strings.Join(warns, " "), "NPM_TOKEN") {
		t.Errorf("the refusal should be reported: %v", warns)
	}
}

// A malformed line may be anything, including a secret printed to the wrong
// descriptor. It is reported by position, never quoted.
func TestMalformedLinesAreNeverQuoted(t *testing.T) {
	_, warns := ParseKeyValueFile([]byte("this is not an assignment\n"), nothingProtected)
	joined := strings.Join(warns, " ")
	if len(warns) == 0 {
		t.Fatal("a malformed line should warn")
	}
	if strings.Contains(joined, "this is not an assignment") {
		t.Errorf("the warning quoted the line: %v", warns)
	}
	if !strings.Contains(joined, "line 1") {
		t.Errorf("the warning should name the position: %v", warns)
	}
}

func TestNonIdentifierKeyIsRefusedWithoutQuoting(t *testing.T) {
	_, warns := ParseKeyValueFile([]byte("not a key=x\n"), nothingProtected)
	if len(warns) == 0 {
		t.Fatal("expected a warning")
	}
	if strings.Contains(strings.Join(warns, " "), "not a key") {
		t.Errorf("a non-identifier key should not be echoed: %v", warns)
	}
}

func TestOversizedFileIsTrimmedAtALineBoundary(t *testing.T) {
	var b strings.Builder
	for b.Len() <= MaxFileBytes+100 {
		b.WriteString("K" + strings.Repeat("x", 60) + "=v\n")
	}
	got, warns := ParseKeyValueFile([]byte(b.String()), nothingProtected)
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "exceeded") {
		t.Errorf("an oversized file should say so: %v", warns)
	}
	for _, a := range got {
		if !keyPattern.MatchString(a.Key) {
			t.Errorf("a partial line survived the cut: %+v", a)
		}
	}
}

func TestParsePathFile(t *testing.T) {
	got, warns := ParsePathFile([]byte("/opt/bin\n\n  /usr/local/bin  \n"))
	if len(warns) != 0 {
		t.Errorf("warnings = %v", warns)
	}
	if len(got) != 2 || got[0] != "/opt/bin" || got[1] != "/usr/local/bin" {
		t.Errorf("paths = %q", got)
	}
}

func TestCRLFIsHandled(t *testing.T) {
	got, _ := ParseKeyValueFile([]byte("FOO=bar\r\nK<<E\r\nv\r\nE\r\n"), nothingProtected)
	if len(got) != 2 || got[0].Value != "bar" || got[1].Value != "v" {
		t.Errorf("CRLF input = %+v", got)
	}
}

// Each $GITHUB_PATH line is prepended in file order, so the last line written
// ends up first — a step that writes two directories and expects the second
// to win relies on it.
func TestAccumulatePathPrependsInFileOrder(t *testing.T) {
	got := AccumulatePath(nil, []string{"/a", "/b"})
	if strings.Join(got, ":") != "/b:/a" {
		t.Errorf("first step = %v, want /b before /a", got)
	}
	got = AccumulatePath(got, []string{"/c"})
	if strings.Join(got, ":") != "/c:/b:/a" {
		t.Errorf("a later step's additions go in front: %v", got)
	}
}

func TestPrependPath(t *testing.T) {
	if got := PrependPath(nil, "/usr/bin"); got != "/usr/bin" {
		t.Errorf("no additions should leave PATH alone: %q", got)
	}
	if got := PrependPath([]string{"/opt/x"}, "/usr/bin"); got != "/opt/x:/usr/bin" {
		t.Errorf("= %q", got)
	}
	// No trailing separator: some shells read "x:" as "x, then here".
	if got := PrependPath([]string{"/opt/x"}, ""); got != "/opt/x" {
		t.Errorf("an empty current PATH must not leave a trailing separator: %q", got)
	}
}

// A51: an output's or a state's name may hold hyphens, as GitHub's do —
// setup-bun writes bun-version and cache-hit — while an environment variable's
// may not.
func TestOutputNamesMayHoldHyphens(t *testing.T) {
	data := []byte("cache-hit=true\nbun-version=1.3.3\n9bad=x\n")
	got, warnings := ParseOutputFile(data)
	if len(got) != 2 || got[0].Key != "cache-hit" || got[1].Value != "1.3.3" {
		t.Errorf("outputs = %+v", got)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %q, want one for the malformed name", warnings)
	}
	env, _ := ParseKeyValueFile(data, nil)
	if len(env) != 0 {
		t.Errorf("an environment file must still refuse hyphens: %+v", env)
	}
}
