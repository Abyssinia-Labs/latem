package result

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// scanFile reads a file of testdata as one stream of one step's output.
// Each file is what the tool printed, on this project's own machines, for the
// broken source its name says; the paths a tool wrote absolute begin
// /work/app.
//
// Captured on 2026-11-12 from cargo and rustc 1.99.1, go 1.26.5, tsc 7.0.2,
// bun 1.3.3, vitest 5.1.0 and biome 2.6.6. A tool that changes how it prints
// fails nothing here: it stops being read, and the fix is a new capture
// beside the old one.
func scanFile(t *testing.T, name string) []Problem {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	s := &scanner{roots: []string{"/work/app"}}
	for _, line := range strings.Split(string(data), "\n") {
		s.line(line)
	}
	s.flush()
	return s.out
}

func check(t *testing.T, name string, want []Problem) {
	t.Helper()
	got := scanFile(t, name)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: read\n%s\nwant\n%s", name, show(got), show(want))
	}
}

func show(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("  " + strings.ReplaceAll(strings.TrimSpace(strings.Join([]string{
			p.Tool, p.Severity, p.File + ":" + itoa(p.Line) + ":" + itoa(p.Column), "[" + p.Code + "]",
			"test=" + p.Test, "msg=" + p.Message, "help=" + p.Help}, " | ")), "\n", `\n`) + "\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func TestScanRustc(t *testing.T) {
	check(t, "cargo-check.txt", []Problem{
		{Severity: "error", File: "src/lib.rs", Line: 3, Column: 13, Code: "E0425",
			Message: "cannot find value `missing` in this scope", Tool: "rustc"},
		{Severity: "error", File: "src/lib.rs", Line: 2, Column: 18, Code: "E0308",
			Message: "mismatched types", Tool: "rustc"},
	})
}

// A warning's fix is the tool's own: beside the code, or on a `help:` of its
// own with the corrected line under it. A lint with no error code is named by
// its lint. cargo's summaries have no place and are not problems.
func TestScanClippy(t *testing.T) {
	check(t, "cargo-clippy.txt", []Problem{
		{Severity: "warning", File: "src/lib.rs", Line: 2, Column: 9, Code: "unused_mut",
			Message: "variable does not need to be mutable", Help: "remove this `mut`", Tool: "rustc"},
		{Severity: "warning", File: "src/lib.rs", Line: 2, Column: 9, Code: "unused_variables",
			Message: "unused variable: `unused`",
			Help:    "if this is intentional, prefix it with an underscore: `_unused`", Tool: "rustc"},
		{Severity: "warning", File: "src/lib.rs", Line: 6, Column: 17, Code: "clippy::ptr_arg",
			Message: "writing `&Vec` instead of `&[_]` involves a new object where a slice will do",
			Help:    "change this to: pub fn first(v: &[i32]) -> i32 {", Tool: "rustc"},
	})
}

func TestScanCargoTest(t *testing.T) {
	got := scanFile(t, "cargo-test.txt")
	var tests []Problem
	for _, p := range got {
		if p.Tool == "cargo-test" {
			tests = append(tests, p)
		}
	}
	want := []Problem{
		{Severity: "error", File: "src/lib.rs", Line: 16, Column: 9, Test: "tests::adds",
			Message: "assertion `left == right` failed\n  left: 2\n right: 3", Tool: "cargo-test"},
		{Severity: "error", File: "src/lib.rs", Line: 7, Column: 6, Test: "tests::panics_plainly",
			Message: "index out of bounds: the len is 0 but the index is 0", Tool: "cargo-test"},
	}
	if !reflect.DeepEqual(tests, want) {
		t.Errorf("read\n%s\nwant\n%s", show(tests), show(want))
	}
	// The build's warnings are in the same output, once for the library and
	// once for its tests; the collector is what makes them one.
	for _, p := range got {
		if p.Tool != "cargo-test" && (p.Tool != "rustc" || p.Severity != "warning") {
			t.Errorf("an unexpected problem: %+v", p)
		}
	}
}

func TestScanGo(t *testing.T) {
	check(t, "go-build.txt", []Problem{
		{Severity: "error", File: "b.go", Line: 3, Column: 8, Message: `"fmt" imported and not used`, Tool: "go"},
		{Severity: "error", File: "b.go", Line: 6, Column: 2, Message: "declared and not used: x", Tool: "go"},
		{Severity: "error", File: "b.go", Line: 6, Column: 7, Message: "undefined: undefinedThing", Tool: "go"},
		{Severity: "error", File: "b.go", Line: 7, Column: 9,
			Message: `cannot use "nope" (untyped string constant) as int value in return statement`, Tool: "go"},
	})
	check(t, "go-vet.txt", []Problem{
		{Severity: "error", File: "c.go", Line: 5, Column: 26,
			Message: `fmt.Printf format %d has arg "x" of wrong type string`, Tool: "go"},
	})
	check(t, "go-vet-build.txt", []Problem{
		{Severity: "error", File: "b.go", Line: 6, Column: 7, Message: "undefined: undefinedThing", Tool: "go"},
	})
}

// A failed test is its name, what it printed, and the last place it named. A
// parent whose subtest failed is not a second failure. With -v the output
// comes before the verdict, and is the same failure.
func TestScanGoTest(t *testing.T) {
	want := []Problem{
		{Severity: "error", File: "a_test.go", Line: 8, Test: "TestAdd",
			Message: "a log line\nAdd(1, 1) = 2, want 3", Tool: "go-test"},
		{Severity: "error", File: "a_test.go", Line: 16, Test: "TestTable/bad",
			Message: "subtest bad failed\nwith a second line", Tool: "go-test"},
	}
	check(t, "go-test.txt", want)
	check(t, "go-test-v.txt", want)
}

func TestScanTsc(t *testing.T) {
	want := []Problem{
		{Severity: "error", File: "a.ts", Line: 3, Column: 9, Code: "TS2322",
			Message: "Type 'string' is not assignable to type 'number'.", Tool: "tsc"},
		{Severity: "error", File: "a.ts", Line: 4, Column: 18, Code: "TS2304",
			Message: "Cannot find name 'missing'.", Tool: "tsc"},
	}
	check(t, "tsc.txt", want)
	// The same, in colour.
	check(t, "tsc-pretty.txt", want)
}

// A path a tool wrote absolute is made relative to the workspace.
func TestScanBunTest(t *testing.T) {
	check(t, "bun-test.txt", []Problem{
		{Severity: "error", File: "a.test.ts", Line: 5, Column: 19, Test: "math > adds",
			Message: "expect(received).toBe(expected)\nExpected: 3\nReceived: 2", Tool: "bun-test"},
		{Severity: "error", File: "a.test.ts", Line: 8, Column: 27, Test: "math > throws",
			Message: "boom", Tool: "bun-test"},
	})
}

func TestScanVitest(t *testing.T) {
	check(t, "vitest.txt", []Problem{
		{Severity: "error", File: "v.test.ts", Line: 5, Column: 19, Test: "math > adds",
			Message: "AssertionError: expected 2 to be 3 // Object.is equality", Tool: "vitest"},
	})
}

func TestScanBiome(t *testing.T) {
	got := scanFile(t, "biome.txt")
	if len(got) < 3 {
		t.Fatalf("read %d problems, want at least 3:\n%s", len(got), show(got))
	}
	want := []Problem{
		{Severity: "warning", File: "a.ts", Line: 2, Column: 3, Code: "lint/style/useConst",
			Message: "This let declares a variable that is only assigned once.", Help: "Use const instead.", Tool: "biome"},
		{Severity: "warning", File: "a.ts", Line: 2, Column: 7, Code: "lint/correctness/noUnusedVariables",
			Message: "This variable unused is unused.",
			Help:    "If this is intentional, prepend unused with an underscore.", Tool: "biome"},
	}
	if !reflect.DeepEqual(got[:2], want) {
		t.Errorf("read\n%s\nwant\n%s", show(got[:2]), show(want))
	}
}

// The shapes that are one line, from the tools that are not on this machine
// to print them.
func TestScanOneLine(t *testing.T) {
	for line, want := range map[string]Problem{
		"/work/app/Sources/App/View.swift:10:5: error: cannot find 'x' in scope": {
			Severity: "error", File: "Sources/App/View.swift", Line: 10, Column: 5, Message: "cannot find 'x' in scope", Tool: "cc"},
		"src/main.c:3:1: warning: unused variable 'a' [-Wunused-variable]": {
			Severity: "warning", File: "src/main.c", Line: 3, Column: 1, Message: "unused variable 'a' [-Wunused-variable]", Tool: "cc"},
		"lib/x.h:9: fatal error: 'y.h' file not found": {
			Severity: "error", File: "lib/x.h", Line: 9, Message: "'y.h' file not found", Tool: "cc"},
		"internal/run/x.go:12:5: Error return value is not checked (errcheck)": {
			Severity: "error", File: "internal/run/x.go", Line: 12, Column: 5, Code: "errcheck",
			Message: "Error return value is not checked", Tool: "go"},
		"FAILED tests/test_math.py::test_adds - assert 2 == 3": {
			Severity: "error", File: "tests/test_math.py", Test: "test_adds", Message: "assert 2 == 3", Tool: "pytest"},
		"thread 'tests::old' panicked at 'explicit panic', src/lib.rs:4:9": {
			Severity: "error", File: "src/lib.rs", Line: 4, Column: 9, Test: "tests::old", Message: "explicit panic", Tool: "cargo-test"},
	} {
		s := &scanner{roots: []string{"/work/app"}}
		s.line(line)
		s.flush()
		if len(s.out) != 1 || s.out[0] != want {
			t.Errorf("%q: read\n%swant\n%s", line, show(s.out), show([]Problem{want}))
		}
	}
}

// What looks a little like a problem and is not one: none of these names a
// place the way a tool that found something does.
func TestScanLeavesOrdinaryOutputAlone(t *testing.T) {
	s := &scanner{}
	for _, line := range []string{
		"error: could not compile `sample` (lib) due to 2 previous errors",
		"error: test failed, to rerun pass `--lib`",
		"warning: `sample` (lib) generated 2 warnings",
		"main.go:12: starting the server", // log.Lshortfile: no column
		"2026-11-12T10:00:00: error: the database is down",
		"see https://example.com:8080: it is documented there",
		"//example.com:443: error: not a file",
		"    Finished `dev` profile [unoptimized + debuginfo] target(s) in 0.05s",
		"test tests::passes ... ok",
		"(pass) math > passes [0.02ms]",
		"error: something a script printed",
		"then an ordinary line",
		"ERROR: 3 retries left",
		"a.go:1:2:", // nothing after the place
	} {
		s.line(line)
	}
	s.flush()
	if len(s.out) != 0 {
		t.Errorf("read problems out of ordinary output:\n%s", show(s.out))
	}
}
