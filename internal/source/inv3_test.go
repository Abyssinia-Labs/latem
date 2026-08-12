package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sentinelName is what a hostile value would create if any of it ever reached
// a shell. Every payload below writes this file; every test asserts it is
// absent afterwards.
const sentinelName = "PWNED"

// assertNoSentinel fails if any hostile payload managed to execute.
func assertNoSentinel(t *testing.T, root string) {
	t.Helper()
	found := []string{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not evidence either way; keep walking.
			return nil
		}
		if strings.Contains(d.Name(), sentinelName) {
			found = append(found, path)
		}
		return nil
	})
	// The payloads also aim outside the fixture, so check the obvious targets.
	for _, p := range []string{
		filepath.Join(os.TempDir(), sentinelName),
		filepath.Join(root, "..", sentinelName),
	} {
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		t.Fatalf("a hostile value reached a shell: %v", found)
	}
}

// hostileRefs is the RUNNER_SPEC §5 table, extended with payloads that create
// [sentinelName] if a shell ever sees them.
//
// registry.ValidateRef rejects every one of these, and that is the point:
// these calls prove the rejection happens at *this* package's call sites too,
// before an argv is built. INV-3 is not satisfied by someone upstream having
// validated — the note in pkg/protocol says it plainly, one validation point
// that everything else trusts is the shape of the next incident.
func hostileRefs(root string) map[string]string {
	touch := "touch " + filepath.Join(root, sentinelName)
	return map[string]string{
		"git option injection":     "--upload-pack=/bin/sh",
		"upload pack with payload": "--upload-pack=" + touch,
		"exec flag":                "--exec=/bin/sh",
		"short option":             "-x",
		"command separator":        "main; " + touch,
		"command substitution":     "$(" + touch + ")",
		"backtick substitution":    "`" + touch + "`",
		"pipe":                     "main | " + touch,
		"logical and":              "main && " + touch,
		"subshell":                 "(" + touch + ")",
		"variable expansion":       "${HOME}",
		"path traversal":           "../../etc/passwd",
		"traversal mid-ref":        "refs/heads/../../../etc/passwd",
		"embedded newline":         "refs/heads/x\nrefs/heads/y",
		"carriage return":          "refs/heads/x\rrefs/heads/y",
		"null byte":                "refs/heads/x\x00y",
		"reflog syntax":            "HEAD@{1}",
		"leading slash":            "/refs/heads/main",
		"trailing slash":           "refs/heads/main/",
		"empty component":          "refs/heads//main",
		"trailing dot":             "refs/heads/main.",
		"dot component":            "refs/heads/.hidden",
		"lock suffix":              "refs/heads/main.lock",
		"space":                    "refs/heads/my branch",
		"tab":                      "refs/heads/my\tbranch",
		"glob star":                "refs/heads/*",
		"tilde":                    "main~1",
		"caret":                    "main^",
		"refspec colon":            "main:refs/heads/evil",
		// Escaped rather than written literally: an invisible character in
		// source is exactly the Trojan Source problem these rows test for, and
		// a reviewer should be able to see what the test contains.
		"cyrillic homoglyph":     "refs/heads/m\u0430in",
		"zero width space":       "refs/heads/ma\u200bin",
		"right-to-left override": "refs/heads/ma\u202ein",
		"ten kilobyte ref":       strings.Repeat("a", 10*1024),
		"empty":                  "",
	}
}

// TestHostileRefs_ReachNoShell drives every entry point that takes a ref.
func TestHostileRefs_ReachNoShell(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()
	sha, err := mirror.Resolve(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for name, ref := range hostileRefs(f.root) {
		t.Run(name, func(t *testing.T) {
			if _, err := mirror.HasRef(ctx, ref); err == nil {
				t.Errorf("HasRef accepted %q", ref)
			}
			if _, err := mirror.Resolve(ctx, ref); err == nil {
				t.Errorf("Resolve accepted %q", ref)
			}
			if _, err := mirror.Acquire(ctx, "run_hostile", ref); err == nil {
				t.Errorf("Acquire accepted %q", ref)
			}
			if _, err := mirror.Checkout(ctx, "run_hostile", ref, sha); err == nil {
				t.Errorf("Checkout accepted %q", ref)
			}
		})
	}

	assertNoSentinel(t, f.root)

	// Nor may a rejected ref have produced a workspace.
	names, err := f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("hostile refs produced workspaces: %v", names)
	}
}

// TestCommandIsArgvNotAShell inspects what the one exec site actually builds.
//
// The tests above prove hostile values are rejected. This one proves the layer
// underneath them would not be exploitable even if a validator were weakened:
// the process executed is git, argv is a slice, and a payload passes through
// as exactly one element with no interpretation.
func TestCommandIsArgvNotAShell(t *testing.T) {
	f := newFixture(t)
	payload := "; touch " + filepath.Join(f.root, sentinelName) + " #"

	cmd := f.mgr.command(context.Background(), "/some/dir", []string{"rev-parse", "--end-of-options", payload})

	if filepath.Base(cmd.Path) != "git" {
		t.Fatalf("executing %q, want git", cmd.Path)
	}
	want := []string{cmd.Path, "-C", "/some/dir", "rev-parse", "--end-of-options", payload}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv = %q, want %q", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.Stdin != nil {
		t.Error("git was given stdin; a credential prompt could block a run forever")
	}
}

// TestCommandClearsInheritedGitState. If the agent is ever started from inside
// a git hook, an inherited GIT_DIR silently overrides the "-C <path>" on every
// command in this package and a run fetches somewhere entirely unrelated.
func TestCommandClearsInheritedGitState(t *testing.T) {
	// The fixture is built first: these variables would derail the plain git
	// calls that construct it, which is itself a demonstration of the problem.
	f := newFixture(t)

	t.Setenv("GIT_DIR", "/somewhere/else/.git")
	t.Setenv("GIT_WORK_TREE", "/somewhere/else")
	t.Setenv("GIT_INDEX_FILE", "/somewhere/else/index")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	cmd := f.mgr.command(context.Background(), f.mgr.MirrorDir(), []string{"status"})

	for _, banned := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE="} {
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, banned) {
				t.Errorf("inherited %q into a git invocation", kv)
			}
		}
	}
	prompts := 0
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			prompts++
			if kv != "GIT_TERMINAL_PROMPT=0" {
				t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", kv)
			}
		}
	}
	if prompts != 1 {
		t.Errorf("GIT_TERMINAL_PROMPT appears %d times, want exactly once", prompts)
	}
}

// TestEveryGitInvocationSeparatesOptionsFromArguments walks the argv of an
// entire acquisition and checks INV-3's rule directly: every element carrying
// a value from outside this package — a ref, a resolved SHA, a repo URL, a
// path built from a run id — is preceded by "--" or "--end-of-options".
//
// Without the separator, "--upload-pack=/bin/sh" as a ref is not a validation
// bug, it is remote code execution, and the validators are one refactor away
// from being the only thing preventing it.
func TestEveryGitInvocationSeparatesOptionsFromArguments(t *testing.T) {
	f := newFixture(t)
	f.up.annotatedTag("v3.0", "release three")

	var argvs [][]string
	f.mgr.onGit = func(argv []string) { argvs = append(argvs, append([]string(nil), argv...)) }

	ws := f.acquire("run_argv", "refs/tags/v3.0")

	// Values that originate outside this package, or that it built from such a
	// value. Every appearance of one in an argv must sit after a separator.
	external := []string{"refs/tags/v3.0", ws.SHA, f.repo.URL, ws.Path}

	if len(argvs) == 0 {
		t.Fatal("no git invocations were recorded")
	}
	for _, argv := range argvs {
		// "-C <dir>" is a global git option and cannot follow a separator. The
		// path is one this package built from a validated key or run id and
		// then asserted stayed inside its directory.
		body := argv
		if len(body) >= 2 && body[0] == "-C" {
			body = body[2:]
		}

		sep := -1
		for i, a := range body {
			if a == "--" || a == "--end-of-options" {
				sep = i
				break
			}
		}
		for i, a := range body {
			// A resolved object name needs no terminator, and on git 2.43
			// cannot have one: `checkout --detach --end-of-options <sha>` fails
			// there with "--detach does not take a path argument" (A40).
			//
			// Exempt by SHAPE. ^[0-9a-f]{40}$ cannot begin with a dash, so git
			// cannot read it as an option wherever it sits, and validateSHA
			// enforces exactly that pattern before the value reaches an argv.
			// A ref, a URL or a path is a string whose characters an attacker
			// chooses — "--upload-pack=/bin/sh" is not hex — so none of those
			// are exempt and all still fail here without a separator.
			if shaPattern.MatchString(a) {
				continue
			}
			for _, value := range external {
				if !strings.Contains(a, value) {
					continue
				}
				if sep < 0 {
					t.Errorf("argv %q carries %q with no -- or --end-of-options before it", argv, value)
				} else if i < sep {
					t.Errorf("argv %q carries %q at position %d, before the separator at %d", argv, value, i, sep)
				}
			}
		}
	}
}

// TestOnlyOneFileInThisPackageCanStartAProcess is a structural test.
//
// INV-3 is auditable because there is exactly one exec site. If a future
// change adds a second one — or reaches for sh -c because quoting was
// fiddly — that property is gone and no behavioural test would notice.
func TestOnlyOneFileInThisPackageCanStartAProcess(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)

		if strings.Contains(text, "os/exec") && name != "git.go" {
			t.Errorf("%s imports os/exec; the exec site belongs in git.go alone", name)
		}
		for _, shell := range []string{"/bin/sh", "/bin/bash", `"sh"`, `"bash"`, "sh -c"} {
			if strings.Contains(text, shell) {
				t.Errorf("%s mentions %s; INV-3 forbids a shell on this path", name, shell)
			}
		}
	}
}
