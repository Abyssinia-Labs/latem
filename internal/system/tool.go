package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Tool is what a probe found out about one external program.
//
// Absence is not an error here. `doctor` on a fresh Mac is supposed to
// run to completion and list everything that is missing, not stop at the first
// one — an operator who has to fix, re-run, fix, re-run has a worse experience
// than one who gets the whole list and does an afternoon's work in one pass.
type Tool struct {
	// Name is the program as spelled on PATH: "git", "docker", "tart".
	Name string
	// Path is where it resolved, empty when absent.
	Path string
	// Found reports whether the program exists on PATH at all.
	Found bool
	// Version is the parsed version, zero when the banner could not be read.
	Version Version
	// Banner is the first line of the version output, verbatim. `doctor`
	// prints it, because "git version 2.50.1 (Apple Git-155)" answers a
	// question the parsed triple does not.
	Banner string
	// Err is why the version could not be established, when Found is true.
	// A tool that exists but will not report its version is usually a broken
	// install or a shim waiting on something, and is worth saying so.
	Err error
}

// Version is a dotted numeric version, parsed loosely from a banner.
//
// Loosely because the three tools this package probes format their banners
// three ways and none of them is stable: "git version 2.50.1 (Apple
// Git-155)", "Docker version 29.6.2, build dfc4efb", and tart's bare "2.28.0".
// Parsing the first x.y[.z] run of digits in the line is what survives all
// three and the next one.
type Version struct {
	Major, Minor, Patch int
	// Text is the matched substring, so a caller can show what was parsed
	// rather than what was inferred.
	Text string
}

// ParseVersion extracts the first dotted numeric version from s.
//
// It returns ok=false rather than a zero Version when there is nothing to
// parse, because "0.0.0" and "no version found" lead to opposite conclusions
// in a minimum-version check: the first fails it, the second should not be
// reported as a version failure at all.
func ParseVersion(s string) (Version, bool) {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			continue
		}
		// A digit preceded by a digit or a dot is inside a run already seen.
		if i > 0 && (isDigit(s[i-1]) || s[i-1] == '.') {
			continue
		}
		v, n, ok := parseAt(s[i:])
		if ok {
			v.Text = s[i : i+n]
			return v, true
		}
	}
	return Version{}, false
}

func parseAt(s string) (Version, int, bool) {
	var parts [3]int
	n, count := 0, 0
	for count < 3 {
		start := n
		for n < len(s) && isDigit(s[n]) {
			n++
		}
		if n == start {
			break
		}
		val, err := strconv.Atoi(s[start:n])
		if err != nil {
			return Version{}, 0, false
		}
		parts[count] = val
		count++
		if count == 3 || n >= len(s) || s[n] != '.' || n+1 >= len(s) || !isDigit(s[n+1]) {
			break
		}
		n++ // consume the dot
	}
	// A bare integer is not a version. "Docker version 29" is not something
	// any of these tools emits, and treating a build number or a year as a
	// version produces a confident wrong answer in a minimum-version check.
	if count < 2 {
		return Version{}, 0, false
	}
	return Version{Major: parts[0], Minor: parts[1], Patch: parts[2]}, n, true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// AtLeast reports whether v is at least the given version.
func (v Version) AtLeast(major, minor, patch int) bool {
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// String renders the parsed triple.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// IsZero reports whether nothing was parsed.
func (v Version) IsZero() bool { return v == Version{} }

// Tool probes one program: is it on PATH, and what does it say its version is.
//
// versionArgs defaults to "--version". `tart` and `xcodebuild` both want
// something else, which is why it is a parameter rather than a constant.
func (s *System) Tool(ctx context.Context, name string, versionArgs ...string) Tool {
	t := Tool{Name: name}
	path, err := s.lookPath(name)
	if err != nil {
		return t
	}
	t.Path, t.Found = path, true

	if len(versionArgs) == 0 {
		versionArgs = []string{"--version"}
	}
	out, err := s.Exec(ctx, name, versionArgs...)
	t.Banner = firstLine(out)
	if err != nil && t.Banner == "" {
		t.Err = err
		return t
	}
	if v, ok := ParseVersion(t.Banner); ok {
		t.Version = v
	} else if err != nil {
		t.Err = err
	}
	return t
}

// ToolAt probes a program at an exact path rather than whatever PATH resolves.
//
// It exists because the interesting case on macOS is a *usable* tool that is
// deliberately not on PATH. Homebrew keeps ruby and openjdk keg-only, so
// `which ruby` reports Apple's 2.6 on a machine that also has 3.4 installed —
// and a check that only asks PATH reports "too old" when the honest answer is
// "the right one is here, it is just not first".
func (s *System) ToolAt(ctx context.Context, path string, versionArgs ...string) Tool {
	t := Tool{Name: filepath.Base(path), Path: path}
	if st, err := os.Stat(path); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return t
	}
	t.Found = true

	if len(versionArgs) == 0 {
		versionArgs = []string{"--version"}
	}
	out, err := s.Exec(ctx, path, versionArgs...)
	t.Banner = firstLine(out)
	if err != nil && t.Banner == "" {
		t.Err = err
		return t
	}
	if v, ok := ParseVersion(t.Banner); ok {
		t.Version = v
	} else if err != nil {
		t.Err = err
	}
	return t
}

// Git probes git. Version matters: RUNNER_SPEC amendment A7 requires
// --end-of-options for every argument position holding a ref, and that option
// landed in git 2.24. An older git makes INV-3's argument-injection defence
// silently weaker, which is not a thing to discover from a build.
func (s *System) Git(ctx context.Context) Tool { return s.Tool(ctx, "git") }

// Docker probes the docker CLI. Daemon reachability is a separate question,
// answered by the executor's own preflight — a CLI with no daemon behind it is
// the single most common "docker is installed" false positive.
func (s *System) Docker(ctx context.Context) Tool { return s.Tool(ctx, "docker") }

// Tart probes tart, whose banner is a bare version string.
func (s *System) Tart(ctx context.Context) Tool { return s.Tool(ctx, "tart", "--version") }

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
