package result

import (
	"regexp"
	"strconv"
	"strings"
)

// scanner reads one stream of one step's output, a line at a time, and keeps
// the problems it names.
//
// Each reader below knows one tool's way of saying what broke and where, as
// that tool prints it today; testdata/ holds what each was written against.
// They all see every line and keep their own state, so a diagnostic that
// spans lines is read by the reader that knows its shape and ignored by the
// rest. None of them guesses: a line that names no file is not a problem
// here, however much it says "error". The log is still there for what these
// miss.
type scanner struct {
	// roots are absolute directories a tool may have prefixed a path with:
	// the workspace as the step saw it. A path under one is made relative.
	roots []string
	out   []Problem

	rust    *Problem // a diagnostic that has its header and may get its place
	rustFix bool     // its help was a `help:` of its own, whose corrected line is still to come

	goCurrent string              // the test verbose output last named
	goLines   map[string][]string // what each test printed before its verdict, verbose
	goFail    *goFailure          // a failed test whose lines are still coming

	libTest  string   // the test whose captured output is being read
	lib      *Problem // a panic that has its place and is collecting its message
	libLines int

	bun bunState

	vitest *Problem // a failed test waiting for its message and place

	biome *Problem // a diagnostic waiting for its message and fix
}

type goFailure struct {
	p     Problem
	lines []string
}

type bunState struct {
	message  string
	expected string
	received string
	file     string
	line     int
	col      int
}

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

	rustHeader = regexp.MustCompile(`^(error|warning)(?:\[([A-Za-z0-9_:]+)\])?: (.+)$`)
	rustPlace  = regexp.MustCompile(`^\s*--> (.+?):(\d+):(\d+)\s*$`)
	rustHelp   = regexp.MustCompile(`\bhelp: (.+)$`)
	rustLint   = regexp.MustCompile("= note: `#\\[(?:warn|deny|forbid)\\(([A-Za-z0-9_:]+)\\)\\]`")
	rustPlus   = regexp.MustCompile(`^\s*\d+\s*\+ ?(.*)$`)

	// file:line:col: error: message, as cc, clang, swiftc and xcodebuild
	// print it, and anything else that follows them.
	ccLine = regexp.MustCompile(`^\s*((?:[A-Za-z]:)?[^\s:(][^:(\n]*):(\d+):(?:(\d+):)?\s*(fatal error|error|warning):\s+(.+)$`)

	tscLine   = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\): (error|warning) (TS\d+): (.+)$`)
	tscPretty = regexp.MustCompile(`^(.+?):(\d+):(\d+) - (error|warning) (TS\d+): (.+)$`)

	// file.go:line:col: message, as the go compiler, vet and the linters
	// built on them print it. The column is required: without one this is
	// also what a program logging with log.Lshortfile prints all day.
	goLine   = regexp.MustCompile(`^(?:vet: )?((?:\.{1,2}/)?[^\s:]+\.go):(\d+):(\d+): (.+)$`)
	goLinter = regexp.MustCompile(` \(([a-z][A-Za-z0-9_-]*)\)$`)

	goRun      = regexp.MustCompile(`^=== (?:RUN|CONT|NAME)\s+(\S+)`)
	goFailLine = regexp.MustCompile(`^\s*--- FAIL: (\S+) \([\d.]+s\)$`)
	goTestLine = regexp.MustCompile(`^\s+(\S+\.go):(\d+): (.*)$`)

	libHeader = regexp.MustCompile(`^---- (.+) stdout ----$`)
	libPanic  = regexp.MustCompile(`^thread '(.+?)'(?: \(\d+\))? panicked at (.+?):(\d+):(\d+):$`)
	libOld    = regexp.MustCompile(`^thread '(.+?)' panicked at '(.*)', (.+?):(\d+):(\d+)$`)

	pytestLine = regexp.MustCompile(`^(?:FAILED|ERROR) (\S+?\.py)::(\S+?)(?: - (.*))?$`)

	bunError  = regexp.MustCompile(`^error: (.+)$`)
	bunFrame  = regexp.MustCompile(`^\s+at .*?\(?((?:/|[A-Za-z]:[\\/])?[^\s()]+?):(\d+):(\d+)\)?$`)
	bunFail   = regexp.MustCompile(`^\(fail\) (.+?)(?: \[[\d.]+\s?m?s\])?$`)
	bunSettle = regexp.MustCompile(`^\((?:pass|skip|todo)\) `)

	vitestFail  = regexp.MustCompile(`^\s*FAIL\s+(\S+) > (.+)$`)
	vitestError = regexp.MustCompile(`^([A-Za-z]*Error): (.+)$`)
	vitestPlace = regexp.MustCompile(`^\s*❯ (\S+?):(\d+):(\d+)$`)

	biomeHeader = regexp.MustCompile(`^(\S+?):(\d+):(\d+) ([a-z][A-Za-z]*(?:/[A-Za-z0-9]+)+)\b.*━`)
	biomeText   = regexp.MustCompile(`^\s+([!×✖✘⚠]) (.+)$`)
	biomeFix    = regexp.MustCompile(`^\s+[iℹ] (?:Safe|Unsafe) fix: (.+)$`)
)

// line reads one line of output.
func (s *scanner) line(text string) {
	text = strings.TrimRight(ansi.ReplaceAllString(text, ""), " \t\r")
	s.rustc(text)
	s.goTest(text)
	s.cargoTest(text)
	s.bunTest(text)
	s.vitestTest(text)
	s.biomeLint(text)
	s.oneLine(text)
}

// flush ends whatever was still being read: the stream closed.
func (s *scanner) flush() {
	s.endRust()
	s.endGoFail()
	s.endLib()
	s.endVitest()
	s.endBiome()
}

func (s *scanner) add(p Problem) {
	p.File = s.relative(p.File)
	p.Message = strings.TrimSpace(p.Message)
	if p.Message == "" {
		return
	}
	s.out = append(s.out, p)
}

// relative makes a path a tool printed one a caller can use: with no ./ in
// front, and relative to the workspace when the tool wrote it absolute.
func (s *scanner) relative(path string) string {
	path = strings.TrimPrefix(path, "./")
	for _, root := range s.roots {
		if root == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(path, strings.TrimRight(root, "/")+"/"); ok {
			return rest
		}
	}
	return path
}

// isSourcePath tells a file a compiler named from the other things that
// come before ":<digits>:": the hour of a timestamp, the host of a URL. A
// source file has a directory or an extension.
func isSourcePath(p string) bool {
	return !strings.HasPrefix(p, "//") && strings.ContainsAny(p, "/.")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// rustc reads rustc's and clippy's diagnostics:
//
//	error[E0308]: mismatched types
//	 --> src/lib.rs:3:18
//	  |
//	3 |     let x: i32 = "a";
//	  |            ---   ^^^ expected `i32`, found `&str`
//
// A header becomes a problem when a `-->` gives it a place, and it ends at
// the blank line rustc puts after each one. cargo's own summaries ("could not
// compile", "generated 2 warnings") have a header and no place, and are not
// kept. Its suggestion is whichever it gave: `help:` beside the code, or a
// `help:` of its own with the corrected line under it.
func (s *scanner) rustc(text string) {
	if text == "" {
		s.endRust()
		return
	}
	if m := rustHeader.FindStringSubmatch(text); m != nil {
		s.endRust()
		s.rust = &Problem{Severity: m[1], Code: m[2], Message: m[3], Tool: "rustc"}
		return
	}
	p := s.rust
	if p == nil {
		return
	}
	if m := rustPlace.FindStringSubmatch(text); m != nil {
		if p.File == "" {
			p.File, p.Line, p.Column = m[1], atoi(m[2]), atoi(m[3])
		}
		return
	}
	if p.File == "" {
		// A header's place is its next line. Anything else there means the
		// header was some other program's line that begins "error:".
		s.rust = nil
		return
	}
	if p.Code == "" {
		if m := rustLint.FindStringSubmatch(text); m != nil {
			p.Code = m[1]
		}
	}
	if m := rustHelp.FindStringSubmatch(text); m != nil && !strings.HasPrefix(m[1], "for further information") {
		if p.Help == "" {
			p.Help = m[1]
			// A `help:` on a line of its own has the corrected code below
			// it, after a gutter line and the line it replaces.
			s.rustFix = strings.HasPrefix(text, "help:")
		}
		return
	}
	if s.rustFix {
		if m := rustPlus.FindStringSubmatch(text); m != nil {
			p.Help = strings.TrimSuffix(p.Help, ":") + ": " + strings.TrimSpace(m[1])
			s.rustFix = false
		}
	}
}

func (s *scanner) endRust() {
	if p := s.rust; p != nil && p.File != "" {
		s.add(*p)
	}
	s.rust, s.rustFix = nil, false
}

// goTest reads `go test`'s failures:
//
//	--- FAIL: TestAdd (0.00s)
//	    a_test.go:8: Add(1, 1) = 2, want 3
//
// The lines under a verdict are what the test printed, and the last that
// names a file is the nearest thing go test has to where it failed. With -v
// they come before the verdict, under `=== RUN`, so they are kept by test
// until one fails. A parent whose subtest failed says nothing of its own and
// is dropped for the subtest.
func (s *scanner) goTest(text string) {
	if m := goRun.FindStringSubmatch(text); m != nil {
		s.endGoFail()
		s.goCurrent = m[1]
		return
	}
	if m := goFailLine.FindStringSubmatch(text); m != nil {
		name := m[1]
		if f := s.goFail; f != nil && strings.HasPrefix(name, f.p.Test+"/") && len(f.lines) == 0 {
			s.goFail = nil
		}
		s.endGoFail()
		f := &goFailure{p: Problem{Severity: SeverityError, Test: name, Tool: "go-test"}}
		f.lines = s.goLines[name]
		delete(s.goLines, name)
		s.goFail = f
		s.goCurrent = ""
		return
	}
	indented := strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\t")
	if !indented {
		if text != "" {
			s.endGoFail()
			s.goCurrent = ""
		}
		return
	}
	if strings.Contains(text, "--- PASS: ") || strings.Contains(text, "--- SKIP: ") {
		return
	}
	switch {
	case s.goFail != nil:
		if len(s.goFail.lines) < 40 {
			s.goFail.lines = append(s.goFail.lines, text)
		}
	case s.goCurrent != "":
		if s.goLines == nil {
			s.goLines = map[string][]string{}
		}
		if len(s.goLines) < 256 && len(s.goLines[s.goCurrent]) < 40 {
			s.goLines[s.goCurrent] = append(s.goLines[s.goCurrent], text)
		}
	}
}

func (s *scanner) endGoFail() {
	f := s.goFail
	if f == nil {
		return
	}
	s.goFail = nil
	var said []string
	for _, l := range f.lines {
		if m := goTestLine.FindStringSubmatch(l); m != nil {
			f.p.File, f.p.Line = m[1], atoi(m[2])
			said = append(said, m[3])
			continue
		}
		said = append(said, strings.TrimSpace(l))
	}
	f.p.Message = strings.Join(said, "\n")
	if f.p.Message == "" {
		f.p.Message = "the test failed and printed nothing"
	}
	s.add(f.p)
}

// cargoTest reads a failed Rust test's panic, from the output libtest prints
// for each failure:
//
//	---- tests::adds stdout ----
//
//	thread 'tests::adds' (85170614) panicked at src/lib.rs:16:9:
//	assertion `left == right` failed
//	  left: 2
//	 right: 3
func (s *scanner) cargoTest(text string) {
	if m := libHeader.FindStringSubmatch(text); m != nil {
		s.endLib()
		s.libTest = m[1]
		return
	}
	if m := libPanic.FindStringSubmatch(text); m != nil {
		s.endLib()
		s.lib = &Problem{Severity: SeverityError, Test: s.libName(m[1]), File: m[2], Line: atoi(m[3]),
			Column: atoi(m[4]), Tool: "cargo-test"}
		s.libLines = 0
		return
	}
	if m := libOld.FindStringSubmatch(text); m != nil {
		s.endLib()
		s.add(Problem{Severity: SeverityError, Test: s.libName(m[1]), Message: m[2], File: m[3], Line: atoi(m[4]),
			Column: atoi(m[5]), Tool: "cargo-test"})
		return
	}
	if s.lib == nil {
		return
	}
	if text == "" || strings.HasPrefix(text, "note: ") || strings.HasPrefix(text, "stack backtrace:") || s.libLines == 8 {
		s.endLib()
		return
	}
	if s.lib.Message != "" {
		s.lib.Message += "\n"
	}
	s.lib.Message += text
	s.libLines++
}

// libName is the test a panic belongs to: the one whose output is being read,
// or the panicking thread's name, which libtest gives each test.
func (s *scanner) libName(thread string) string {
	if s.libTest != "" {
		return s.libTest
	}
	if thread == "main" {
		return ""
	}
	return thread
}

func (s *scanner) endLib() {
	if s.lib != nil {
		if s.lib.Message == "" {
			s.lib.Message = "panicked"
		}
		s.add(*s.lib)
	}
	s.lib = nil
}

// bunTest reads `bun test`, which prints a failure's error and stack and then
// its verdict:
//
//	error: expect(received).toBe(expected)
//	Expected: 3
//	Received: 2
//	      at <anonymous> (/work/app/a.test.ts:5:19)
//	(fail) math > adds [1.70ms]
func (s *scanner) bunTest(text string) {
	if m := bunFail.FindStringSubmatch(text); m != nil {
		msg := s.bun.message
		if s.bun.expected != "" || s.bun.received != "" {
			msg += "\n" + s.bun.expected + "\n" + s.bun.received
		}
		if msg == "" {
			msg = "the test failed"
		}
		s.add(Problem{Severity: SeverityError, Test: m[1], Message: msg, File: s.bun.file, Line: s.bun.line,
			Column: s.bun.col, Tool: "bun-test"})
		s.bun = bunState{}
		return
	}
	if bunSettle.MatchString(text) {
		s.bun = bunState{}
		return
	}
	if m := bunError.FindStringSubmatch(text); m != nil {
		s.bun = bunState{message: m[1]}
		return
	}
	if s.bun.message == "" {
		return
	}
	switch {
	case strings.HasPrefix(text, "Expected: "):
		s.bun.expected = text
	case strings.HasPrefix(text, "Received: "):
		s.bun.received = text
	case s.bun.file == "":
		if m := bunFrame.FindStringSubmatch(text); m != nil {
			s.bun.file, s.bun.line, s.bun.col = m[1], atoi(m[2]), atoi(m[3])
		}
	}
}

// vitestTest reads vitest's report of a failed test:
//
//	 FAIL  v.test.ts > math > adds
//	AssertionError: expected 2 to be 3 // Object.is equality
//	 ❯ v.test.ts:5:19
func (s *scanner) vitestTest(text string) {
	if m := vitestFail.FindStringSubmatch(text); m != nil {
		s.endVitest()
		s.vitest = &Problem{Severity: SeverityError, File: m[1], Test: m[2], Tool: "vitest"}
		return
	}
	p := s.vitest
	if p == nil {
		return
	}
	if m := vitestPlace.FindStringSubmatch(text); m != nil {
		p.File, p.Line, p.Column = m[1], atoi(m[2]), atoi(m[3])
		s.endVitest()
		return
	}
	if p.Message == "" {
		if m := vitestError.FindStringSubmatch(text); m != nil {
			p.Message = m[1] + ": " + m[2]
		}
	}
}

func (s *scanner) endVitest() {
	if p := s.vitest; p != nil {
		if p.Message == "" {
			p.Message = "the test failed"
		}
		s.add(*p)
	}
	s.vitest = nil
}

// biomeLint reads a biome diagnostic:
//
//	a.ts:2:3 lint/style/useConst  FIXABLE  ━━━━━━━━━━
//
//	  ! This let declares a variable that is only assigned once.
//	  …
//	  i Safe fix: Use const instead.
//
// `!` is a warning and `×` an error, as biome marks them.
func (s *scanner) biomeLint(text string) {
	if m := biomeHeader.FindStringSubmatch(text); m != nil {
		s.endBiome()
		s.biome = &Problem{File: m[1], Line: atoi(m[2]), Column: atoi(m[3]), Code: m[4], Tool: "biome"}
		return
	}
	p := s.biome
	if p == nil {
		return
	}
	if text != "" && !strings.HasPrefix(text, " ") {
		s.endBiome()
		return
	}
	if p.Message == "" {
		if m := biomeText.FindStringSubmatch(text); m != nil {
			p.Severity = SeverityError
			if m[1] == "!" || m[1] == "⚠" {
				p.Severity = SeverityWarning
			}
			p.Message = m[2]
		}
		return
	}
	if p.Help == "" {
		if m := biomeFix.FindStringSubmatch(text); m != nil {
			p.Help = m[1]
		}
	}
}

func (s *scanner) endBiome() {
	if p := s.biome; p != nil && p.Message != "" {
		s.add(*p)
	}
	s.biome = nil
}

// oneLine reads the tools that say everything on one line.
func (s *scanner) oneLine(text string) {
	if !strings.Contains(text, ":") {
		return
	}
	if m := tscLine.FindStringSubmatch(text); m != nil {
		s.add(Problem{Severity: m[4], File: m[1], Line: atoi(m[2]), Column: atoi(m[3]), Code: m[5], Message: m[6], Tool: "tsc"})
		return
	}
	if m := tscPretty.FindStringSubmatch(text); m != nil {
		s.add(Problem{Severity: m[4], File: m[1], Line: atoi(m[2]), Column: atoi(m[3]), Code: m[5], Message: m[6], Tool: "tsc"})
		return
	}
	if m := ccLine.FindStringSubmatch(text); m != nil && isSourcePath(m[1]) && atoi(m[2]) > 0 {
		severity := m[4]
		if severity == "fatal error" {
			severity = SeverityError
		}
		s.add(Problem{Severity: severity, File: m[1], Line: atoi(m[2]), Column: atoi(m[3]), Message: m[5], Tool: "cc"})
		return
	}
	if m := goLine.FindStringSubmatch(text); m != nil {
		p := Problem{Severity: SeverityError, File: m[1], Line: atoi(m[2]), Column: atoi(m[3]), Message: m[4], Tool: "go"}
		if l := goLinter.FindStringSubmatch(p.Message); l != nil {
			p.Code = l[1]
			p.Message = strings.TrimSuffix(p.Message, l[0])
		}
		s.add(p)
		return
	}
	if m := pytestLine.FindStringSubmatch(text); m != nil {
		msg := m[3]
		if msg == "" {
			msg = "the test failed"
		}
		s.add(Problem{Severity: SeverityError, File: m[1], Test: m[2], Message: msg, Tool: "pytest"})
	}
}
