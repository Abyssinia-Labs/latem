//go:build unix

package invariants

// INV-3 — Broker-supplied values are never interpolated into a shell.
//
// The validation table lives in internal/registry/ref_test.go and passes. The
// call-site table lives in internal/source/inv3_test.go and passes. Neither is
// repeated here.
//
// What this file adds is the property those two exist to defend, asserted from
// outside both packages and against evidence stronger than a string
// comparison: a real acquisition, driven with hostile refs, hostile repo keys
// and hostile run ids, watched by a sentinel that a successful injection would
// trip.
//
// # Why a sentinel, and why it is proved to work first
//
// internal/source's own INV-3 test asserts that a file named PWNED does not
// appear. That is the right shape and it has one gap: nothing in it ever
// demonstrates that a PWNED file *could* have appeared. If the payloads were
// inert against this git, or the walk looked in the wrong directory, the
// assertion would read exactly the same and mean nothing — the failure
// amendment A25 records for INV-1, and the reason INV-4 shipped green over a
// live leak.
//
// So the detector here is built and then attacked deliberately, twice, before
// it is used to clear anything:
//
//   - a shell is invoked by name and must be seen, which proves the PATH
//     interception is live;
//   - git is handed `--upload-pack=<sentinel>` directly and must run it, which
//     proves the option-injection class is live against this git binary, this
//     repository layout, and this platform.
//
// Only then is the real acquisition path asked to stay clean.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/source"
)

// pwnCommand is a name that exists nowhere on a normal machine.
//
// The hostile payloads below invoke it rather than `touch /tmp/PWNED`, because
// a bare name can only be found through PATH: its execution is unambiguous
// evidence that something resolved a command, where a file appearing is one
// step further removed and one more thing that can go wrong quietly.
const pwnCommand = "latem-inv3-pwn"

// interceptedTools are the programs a successful injection would reach for.
//
// Each one logs and then execs the real program, so planting them cannot
// change what git does — a fake `sh` that refused to work would turn a passing
// invariant into a broken fixture, and the difference would take an afternoon
// to find.
var interceptedTools = []string{"sh", "bash", "dash", "zsh", "id", "curl", "wget", "nc", "touch"}

// sentinel is a doctored PATH plus the log every intercepted program writes to.
type sentinel struct {
	dir string
	log string
	// env is what to hand a subprocess so it resolves the intercepted tools.
	env []string
}

func newSentinel(t *testing.T) *sentinel {
	t.Helper()

	dir := shortTemp(t)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "executions.log")

	plant := func(name, real string) {
		body := "#!/bin/sh\n" +
			fmt.Sprintf("printf '%%s %%s\\n' %q \"$*\" >> %q\n", name, logPath)
		if real != "" {
			body += fmt.Sprintf("exec %q \"$@\"\n", real)
		}
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range interceptedTools {
		real, err := exec.LookPath(name)
		if err != nil {
			real = "" // absent on this machine; log and succeed
		}
		plant(name, real)
	}
	// The payload target itself has no real counterpart, by construction.
	plant(pwnCommand, "")

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	s := &sentinel{dir: dir, log: logPath}
	s.env = append(hermeticGitEnv(), "PATH="+bin+":"+os.Getenv("PATH"))
	return s
}

// fired returns every intercepted execution since the last [sentinel.reset].
func (s *sentinel) fired(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func (s *sentinel) reset(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(s.log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pwnPath is the absolute path of the payload program, for the payloads that
// pretend to be a git option rather than a shell fragment.
func (s *sentinel) pwnPath() string { return filepath.Join(s.dir, "bin", pwnCommand) }

// ---------------------------------------------------------------------------
// the negative controls

// TestINV3_TheSentinelCanFire is the control for everything below it.
//
// Two ways in, both of which must work, or a clean run of
// TestINV3_HostileValuesReachNoShell proves nothing at all.
func TestINV3_TheSentinelCanFire(t *testing.T) {
	requireGit(t)
	s := newSentinel(t)

	t.Run("a shell resolved through PATH is seen", func(t *testing.T) {
		s.reset(t)
		// The outer shell is named absolutely because os/exec resolves the
		// program against the *parent's* PATH, not against cmd.Env — so
		// exec.Command("sh", …) would find the machine's own sh and prove
		// nothing. What is being demonstrated is what happens inside a child:
		// git resolves its own subprocesses against the environment it was
		// given, and that environment is this one.
		cmd := exec.Command("/bin/sh", "-c", "sh -c "+pwnCommand)
		cmd.Env = append(os.Environ(), s.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("running the control: %v\n%s", err, out)
		}
		fired := s.fired(t)
		if !firedIncludes(fired, "sh") {
			t.Errorf("the intercepted sh did not log; PATH interception is not working: %v", fired)
		}
		if !firedIncludes(fired, pwnCommand) {
			t.Errorf("the payload did not log; a shell payload would not be detected: %v", fired)
		}
	})

	t.Run("git runs an --upload-pack it is handed", func(t *testing.T) {
		// This is the attack INV-3's leading-dash rule exists for, executed on
		// purpose so that "the real path did not trip the sentinel" is a fact
		// about the real path rather than about git having changed.
		s.reset(t)

		upstream := shortTemp(t)
		initRepo(t, upstream, declaredJobs)
		mirror := filepath.Join(shortTemp(t), "mirror.git")
		git(t, ".", "clone", "--quiet", "--mirror", upstream, mirror)

		cmd := exec.Command("git", "-C", mirror,
			"fetch", "--upload-pack="+s.pwnPath(), upstream, "main")
		cmd.Env = append(os.Environ(), s.env...)
		out, err := cmd.CombinedOutput()

		fired := s.fired(t)
		if !firedIncludes(fired, pwnCommand) {
			t.Fatalf("git did not execute the program named by --upload-pack, so option "+
				"injection is not demonstrably live here and a clean run below would be "+
				"uninformative.\ngit said: %v\n%s\nsentinel: %v", err, out, fired)
		}
	})
}

// TestINV3_HostileValuesReachNoShell drives a real acquisition with every
// hostile value the trust boundary can carry.
//
// The three fields are driven separately because they enter at different
// places: `ref` reaches four methods on a Mirror, `repo` reaches Manager.Fetch
// as a key and a URL, and a run id becomes the workspace directory name. §5
// enumerates the first two and amendment A6 added the third after `gosec`
// found that `{"id": "../../../../tmp/x"}` wrote outside ~/.latem entirely.
func TestINV3_HostileValuesReachNoShell(t *testing.T) {
	requireGit(t)
	s := newSentinel(t)

	upstream := shortTemp(t)
	initRepo(t, upstream, declaredJobs)

	root := shortTemp(t)
	mgr, err := source.New(source.Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		Env:       s.env,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mirror, err := mgr.Fetch(ctx, source.Repo{Key: "app", URL: upstream})
	if err != nil {
		t.Fatalf("the legitimate fetch failed, so nothing below exercises anything: %v", err)
	}
	sha, err := mirror.Resolve(ctx, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Checkout(ctx, "r_legit", "refs/heads/main", sha); err != nil {
		t.Fatalf("the legitimate checkout failed: %v", err)
	}
	if fired := s.fired(t); len(fired) > 0 {
		t.Fatalf("a legitimate acquisition reached %d intercepted programs, so the sentinel "+
			"cannot distinguish an injection from ordinary work: %v", len(fired), fired)
	}

	// --- refs ---------------------------------------------------------------
	for name, ref := range hostileRefs(s) {
		t.Run("ref/"+name, func(t *testing.T) {
			s.reset(t)
			if _, err := mirror.HasRef(ctx, ref); err == nil {
				t.Errorf("HasRef accepted %q", ref)
			}
			if _, err := mirror.Resolve(ctx, ref); err == nil {
				t.Errorf("Resolve accepted %q", ref)
			}
			if _, err := mirror.Acquire(ctx, "r_hostileref", ref); err == nil {
				t.Errorf("Acquire accepted %q", ref)
			}
			if _, err := mirror.Checkout(ctx, "r_hostileref", ref, sha); err == nil {
				t.Errorf("Checkout accepted %q", ref)
			}
			assertNothingExecuted(t, s, "ref "+name)
		})
	}

	// --- repo keys and URLs -------------------------------------------------
	for name, repo := range hostileRepos(s) {
		t.Run("repo/"+name, func(t *testing.T) {
			s.reset(t)
			mi, err := mgr.Fetch(ctx, repo)
			if err == nil && mi != nil {
				// A key the manager accepts must still not have escaped its
				// directory, and must still not have run anything.
				t.Errorf("Fetch accepted key=%q url=%q", repo.Key, repo.URL)
			}
			assertNothingExecuted(t, s, "repo "+name)
		})
	}

	// --- run ids ------------------------------------------------------------
	for name, id := range hostileRunIDs() {
		t.Run("runid/"+name, func(t *testing.T) {
			s.reset(t)
			if _, err := mirror.Acquire(ctx, id, "refs/heads/main"); err == nil {
				t.Errorf("Acquire accepted run id %q", id)
			}
			if _, err := mirror.Checkout(ctx, id, "refs/heads/main", sha); err == nil {
				t.Errorf("Checkout accepted run id %q", id)
			}
			assertNothingExecuted(t, s, "run id "+name)
		})
	}

	// Nothing hostile may have left a directory behind. ListWorkspaces returns
	// names it did not create too, which is the point: hiding them would hide
	// a leak.
	names, err := mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n != "r_legit" {
			t.Errorf("a hostile value produced the workspace %q", n)
		}
	}

	// And nothing may have been written outside the work directory. The run id
	// payloads aim at the fixture root and at /tmp, which is where a traversal
	// from work/<id> lands.
	for _, p := range []string{
		filepath.Join(root, pwnCommand),
		filepath.Join(root, "..", pwnCommand),
		filepath.Join(os.TempDir(), pwnCommand),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a hostile value produced %s", p)
		}
	}
}

// TestINV3_EveryGitArgumentFromOutsideSitsAfterATerminator audits the argv of a
// whole successful acquisition.
//
// The hostile table above proves the validators reject. This proves the layer
// underneath them is not exploitable if one of those validators is ever
// weakened, which is the actual content of INV-3's last paragraph: `--` or
// `--end-of-options` before any argument a broker could have influenced, and
// which of the two is not a style choice (amendment A7 — `git checkout
// --detach -- $SHA` does not run).
//
// It is asserted from outside internal/source through the [source.Config]
// GitBinary seam, which already exists for the LFS tests, so no new export was
// needed to see the argv.
func TestINV3_EveryGitArgumentFromOutsideSitsAfterATerminator(t *testing.T) {
	requireGit(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	root := shortTemp(t)
	argvLog := filepath.Join(root, "argv.log")
	shim := filepath.Join(root, "git-shim")
	// NUL-separated, because a ref in this table may legitimately contain a
	// newline and a line-oriented log would split one argument into two.
	body := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\0' \"$a\" >> %q; done\n"+
		"printf '\\1' >> %q\nexec %q \"$@\"\n", argvLog, argvLog, realGit)
	if err := os.WriteFile(shim, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	upstream := shortTemp(t)
	initRepo(t, upstream, declaredJobs)
	git(t, upstream, "tag", "-a", "v1.0", "-m", "release one")

	mgr, err := source.New(source.Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		GitBinary: shim,
		Env:       hermeticGitEnv(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mirror, err := mgr.Fetch(ctx, source.Repo{Key: "app", URL: upstream})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := mirror.Acquire(ctx, "r_argv", "refs/tags/v1.0")
	if err != nil {
		t.Fatal(err)
	}

	invocations := readArgvLog(t, argvLog)
	if len(invocations) == 0 {
		t.Fatal("the shim recorded no git invocations; the seam is not in the path and every " +
			"assertion below would pass against anything")
	}

	// Values that came from outside this package, or that it built from one.
	external := []string{"refs/tags/v1.0", ws.SHA, upstream, ws.Path}

	sawTerminator := false
	for _, argv := range invocations {
		// "-C <dir>" is a global git option and cannot follow a separator.
		// The path is one internal/source built from a validated repo key or
		// run id and then asserted stayed inside its own directory, so it is
		// not an argument a broker influenced — it is a directory this machine
		// chose.
		body := argv
		if len(body) >= 2 && body[0] == "-C" {
			body = body[2:]
		}

		for i, arg := range body {
			if arg == "--" || arg == "--end-of-options" {
				sawTerminator = true
			}
			if !isExternal(arg, external) {
				continue
			}
			if !terminatedBefore(body, i) {
				t.Errorf("git %s: the external value %q at position %d is not preceded by \"--\" "+
					"or \"--end-of-options\" (INV-3)", strings.Join(argv, " "), arg, i)
			}
		}
		// A shell must never be the program, and never an argument.
		for _, arg := range argv {
			if shellNames.MatchString(arg) {
				t.Errorf("git %s: a shell appears in the argv", strings.Join(argv, " "))
			}
		}
	}
	if !sawTerminator {
		t.Error("no invocation in a whole acquisition used an options terminator; either the " +
			"acquisition did nothing or the terminators are gone")
	}
}

// ---------------------------------------------------------------------------
// tables

// hostileRefs is RUNNER_SPEC §5's table, with payloads that invoke
// [pwnCommand] rather than merely looking dangerous.
func hostileRefs(s *sentinel) map[string]string {
	return map[string]string{
		"upload-pack option":     "--upload-pack=" + s.pwnPath(),
		"upload-pack to sh":      "--upload-pack=/bin/sh",
		"exec option":            "--exec=" + s.pwnPath(),
		"receive-pack option":    "--receive-pack=" + s.pwnPath(),
		"config option":          "--config=core.pager=" + s.pwnPath(),
		"short option":           "-x",
		"command separator":      "main; " + pwnCommand,
		"command substitution":   "$(" + pwnCommand + ")",
		"backtick substitution":  "`" + pwnCommand + "`",
		"pipe":                   "main | " + pwnCommand,
		"logical and":            "main && " + pwnCommand,
		"subshell":               "(" + pwnCommand + ")",
		"newline then payload":   "refs/heads/main\n" + pwnCommand,
		"carriage return":        "refs/heads/x\rrefs/heads/y",
		"null byte":              "refs/heads/x\x00y",
		"variable expansion":     "${HOME}",
		"path traversal":         "../../etc/passwd",
		"traversal mid-ref":      "refs/heads/../../../etc/passwd",
		"reflog syntax":          "HEAD@{1}",
		"refspec colon":          "main:refs/heads/evil",
		"glob star":              "refs/heads/*",
		"tilde":                  "main~1",
		"caret":                  "main^",
		"leading slash":          "/refs/heads/main",
		"space":                  "refs/heads/my branch",
		"tab":                    "refs/heads/my\tbranch",
		"cyrillic homoglyph":     "refs/heads/mаin",
		"zero width space":       "refs/heads/ma\u200bin",
		"right-to-left override": "refs/heads/ma\u202ein",
		"ten kilobyte ref":       strings.Repeat("a", 10*1024),
		"empty":                  "",
	}
}

// hostileRepos drives both halves of a repo entry. INV-3 says the broker's
// `repo` is matched exactly against the config map and never used to build a
// path; what reaches internal/source is the key that match produced, so these
// are what a config-matching bug would let through.
func hostileRepos(s *sentinel) map[string]source.Repo {
	return map[string]source.Repo{
		"key traversal":     {Key: "../../etc", URL: "https://example.invalid/x.git"},
		"key with a slash":  {Key: "a/b", URL: "https://example.invalid/x.git"},
		"key absolute":      {Key: "/etc/passwd", URL: "https://example.invalid/x.git"},
		"key with a null":   {Key: "a\x00b", URL: "https://example.invalid/x.git"},
		"key is a dot":      {Key: "..", URL: "https://example.invalid/x.git"},
		"key with payload":  {Key: "app; " + pwnCommand, URL: "https://example.invalid/x.git"},
		"key empty":         {Key: "", URL: "https://example.invalid/x.git"},
		"url option":        {Key: "app2", URL: "--upload-pack=" + s.pwnPath()},
		"url ext transport": {Key: "app3", URL: "ext::" + s.pwnPath() + " %S"},
		"url with payload":  {Key: "app4", URL: "https://example.invalid/x.git; " + pwnCommand},
		"url empty":         {Key: "app5", URL: ""},
	}
}

// hostileRunIDs is amendment A6's table. Every one of these becomes
// work/<id> if it is not refused.
func hostileRunIDs() map[string]string {
	return map[string]string{
		"traversal":     "../../../../tmp/" + pwnCommand,
		"dot dot":       "..",
		"dot":           ".",
		"slash":         "r_a/b",
		"null":          "r_a\x00b",
		"space":         "r_a b",
		"leading dash":  "-rf",
		"substitution":  "$(" + pwnCommand + ")",
		"separator":     "r_a; " + pwnCommand,
		"empty":         "",
		"contains dot":  "r_a.b",
		"far too long":  "r_" + strings.Repeat("a", 200),
		"tilde":         "~/" + pwnCommand,
		"absolute path": "/tmp/" + pwnCommand,
	}
}

// ---------------------------------------------------------------------------
// helpers

func assertNothingExecuted(t *testing.T, s *sentinel, what string) {
	t.Helper()
	if fired := s.fired(t); len(fired) > 0 {
		t.Errorf("%s reached a shell or an intercepted program (INV-3): %v", what, fired)
	}
}

func firedIncludes(fired []string, name string) bool {
	for _, line := range fired {
		if strings.HasPrefix(line, name+" ") {
			return true
		}
	}
	return false
}

// readArgvLog splits the shim's NUL-separated argument stream back into one
// slice per invocation. \1 terminates an invocation.
func readArgvLog(t *testing.T, path string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the argv log: %v", err)
	}
	var out [][]string
	for _, chunk := range strings.Split(string(b), "\x01") {
		if chunk == "" {
			continue
		}
		var argv []string
		for _, a := range strings.Split(chunk, "\x00") {
			if a != "" {
				argv = append(argv, a)
			}
		}
		if len(argv) > 0 {
			out = append(out, argv)
		}
	}
	return out
}

// isExternal reports whether arg carries a value from outside internal/source.
func isExternal(arg string, external []string) bool {
	for _, e := range external {
		if e != "" && strings.Contains(arg, e) {
			return true
		}
	}
	return false
}

// terminatedBefore reports whether an options terminator appears before i.
func terminatedBefore(argv []string, i int) bool {
	for j := range i {
		if argv[j] == "--" || argv[j] == "--end-of-options" {
			return true
		}
	}
	return false
}
