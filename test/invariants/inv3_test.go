package invariants

// INV-3, the portable half: nothing anywhere reaches `sh -c` with a
// broker-supplied value.
//
// The other half — driving hostile refs, repo keys and run ids through a real
// git acquisition behind a sentinel — is in inv3_unix_test.go, because its
// detector is a shell planted on PATH and the negative control that proves the
// detector works has to execute one.
//
// This file is the structural claim, and it is here rather than in
// internal/source because it is the only one of the two that is about the
// whole tree. internal/source already pins that only git.go may import
// os/exec; that says nothing about a shell appearing in internal/cache, in
// internal/system, or in a package that does not exist yet.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// shellNames are the argv[0] values that mean "a shell is about to interpret
// the next argument".
var shellNames = regexp.MustCompile(`^(?:/(?:usr/)?bin/)?(?:sh|bash|zsh|dash|ksh|fish)$`)

// shellCFlag matches the flag that turns a shell into an interpreter of a
// string: -c, and bash's login variant -lc.
var shellCFlag = regexp.MustCompile(`^-[a-z]*c$`)

// shellCallSites is every non-test file permitted to build a shell invocation,
// with the reason each is legitimate.
//
// This is an allowlist rather than a count because a count tells a reviewer
// that something moved and not what. Adding an entry here is the moment to ask
// the INV-3 question — can any part of that argv come from outside the trust
// boundary? — and the answer for all of these is no: four build fixed
// literals, and the fifth passes a registry-declared step body as a single
// argv element, which RUNNER_SPEC §9 requires and §5 explicitly permits
// because the registry is read off disk at the resolved commit (INV-5) and
// never from a broker (INV-2).
var shellCallSites = map[string]string{
	"internal/executor/docker/env.go": "§9's step contract: []string{shell, \"-c\", step.Run}, " +
		"where step.Run is a registry literal and is one argv element",
	"internal/executor/native/env.go": "§9's step contract: []string{shell, \"-lc\", step.Run}, " +
		"the same, with a login shell because §13.3 needs the operator's PATH",
	"internal/executor/docker/image.go": "a fixed image-probe script, no interpolation",
	"internal/executor/docker/executor.go": "two fixed literals: the idle entrypoint and the " +
		"/dev/kvm readability probe",
	"cmd/latem/checks_locale.go": "A33's locale probe: `env LANG=… bash -lc <fixed script>`, " +
		"every element a compile-time constant. It runs the step's own shell deliberately, " +
		"because a login shell's profile is what can overrule the locale and only the shell " +
		"can report the result — but nothing from any run reaches this argv, and `doctor` " +
		"takes no broker input at all",
}

// TestINV3_NoUnexpectedShellInvocation walks every non-test Go file in the
// module and fails on a shell invocation outside [shellCallSites].
//
// It reads the AST rather than the text, so a comment describing a shell —
// internal/secrets/runner.go has one, explaining that §15's commands are argv
// arrays precisely so this never happens — is not a hit. A regex over source
// would flag it, and a test that cries wolf on documentation gets an exception
// added until it flags nothing at all.
func TestINV3_NoUnexpectedShellInvocation(t *testing.T) {
	root := moduleRoot(t)

	// Negative control. The detector is a small AST walk with two regexes in
	// it, and either could be wrong in a way that finds nothing anywhere —
	// which is indistinguishable from a clean tree. So it is first run against
	// a file that is known to contain the thing being looked for.
	control := filepath.Join(root, "internal/executor/native/env.go")
	if got := shellSitesIn(t, control); len(got) == 0 {
		t.Fatalf("the detector found no shell invocation in %s, which contains one; every "+
			"assertion below would pass against anything", control)
	}

	var offences []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "node_modules", "examples", "test":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if _, allowed := shellCallSites[rel]; allowed {
			return nil
		}
		for _, site := range shellSitesIn(t, path) {
			offences = append(offences, rel+": "+site)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, o := range offences {
		t.Errorf("a shell invocation outside the sanctioned sites (INV-3): %s\n"+
			"if this is legitimate, add it to shellCallSites with the reason no part of its argv "+
			"can come from outside the trust boundary", o)
	}

	// An allowlist entry whose site has gone is debt that hides the next one.
	for rel := range shellCallSites {
		if got := shellSitesIn(t, filepath.Join(root, rel)); len(got) == 0 {
			t.Errorf("shellCallSites still lists %s, which no longer invokes a shell; remove the "+
				"entry so the next one that appears there is caught", rel)
		}
	}
}

// shellSitesIn returns a description of every shell invocation in one file.
func shellSitesIn(t *testing.T, path string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	var out []string
	record := func(pos token.Pos, argv []string) {
		out = append(out, fset.Position(pos).String()+" "+strings.Join(argv, " "))
	}

	ast.Inspect(file, func(n ast.Node) bool {
		var args []ast.Expr
		switch v := n.(type) {
		case *ast.CompositeLit:
			args = v.Elts
		case *ast.CallExpr:
			args = v.Args
		default:
			return true
		}
		// A shell invocation is a program immediately followed by a -c flag.
		//
		// The flag has to be the literal, not the program: the two step
		// contracts in the executors are `[]string{shell, "-c", step.Run}`,
		// where `shell` is a variable holding the registry's `shell:` value.
		// A detector keyed on the program name finds neither of them and
		// reports a clean tree having looked only for a spelling nobody uses —
		// which is why the control above exists and why it caught exactly that
		// while this was being written.
		//
		// Requiring adjacency is what keeps `[]string{"bash", "git"}` —
		// docker's tool probe — from reading as one.
		for i := 0; i+1 < len(args); i++ {
			flag, ok := stringLit(args[i+1])
			if !ok || !shellCFlag.MatchString(flag) {
				continue
			}
			switch prog := args[i].(type) {
			case *ast.BasicLit:
				if name, ok := stringLit(prog); ok && shellNames.MatchString(name) {
					record(args[i].Pos(), []string{name, flag})
				}
			case *ast.Ident:
				record(args[i].Pos(), []string{prog.Name, flag})
			case *ast.SelectorExpr:
				record(args[i].Pos(), []string{prog.Sel.Name, flag})
			}
		}
		return true
	})
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// moduleRoot walks up from this package to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
