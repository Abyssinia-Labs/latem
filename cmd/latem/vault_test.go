package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeVaultDir is a directory holding an executable script for each name.
func fakeVaultDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		script := "#!/bin/sh\necho \"" + n + " $*\"\n"
		if err := os.WriteFile(filepath.Join(dir, n), []byte(script), 0o755); err != nil { // #nosec G306 -- a test's program
			t.Fatal(err)
		}
	}
	return dir
}

// executeCommand runs latem with args, as it is typed.
func executeCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// onlyIn makes the vault be looked for in dirs alone, whatever this machine
// has installed, and records what would be run instead of running it.
func onlyIn(t *testing.T, dirs ...string) *[]string {
	t.Helper()
	var ran []string
	oldDirs, oldExec := vaultSearchDirs, vaultExec
	vaultSearchDirs = func([]string) []string { return dirs }
	vaultExec = func(path string, args, _ []string) error {
		ran = append([]string{path}, args...)
		return nil
	}
	t.Cleanup(func() { vaultSearchDirs, vaultExec = oldDirs, oldExec })
	return &ran
}

// A110, A113: `latem vault …` runs pinkvault with the same arguments, flags
// included, and takes none of them for itself, though the old names are
// here too and in an earlier directory.
func TestVault_RunsPinkvaultWithEveryArgument(t *testing.T) {
	old := fakeVaultDir(t, "latem-vault", "openvault")
	dir := fakeVaultDir(t, "pinkvault")
	ran := onlyIn(t, t.TempDir(), old, dir)
	if _, err := executeCommand(t, "vault", "collections", "--json", "--help"); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dir, "pinkvault"), "collections", "--json", "--help"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %q, want %q", *ran, want)
	}
}

// A110: a machine whose vault is still latem-vault runs that (A104).
func TestVault_RunsLatemVaultWhenThatIsTheNewest(t *testing.T) {
	dir := fakeVaultDir(t, "latem-vault", "openvault")
	ran := onlyIn(t, dir)
	if _, err := executeCommand(t, "vault", "list"); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dir, "latem-vault"), "list"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %q, want %q", *ran, want)
	}
}

// A110: a machine with only the old name runs that (A104).
func TestVault_RunsOpenvaultWhenThatIsAllThereIs(t *testing.T) {
	dir := fakeVaultDir(t, "openvault")
	ran := onlyIn(t, dir)
	if _, err := executeCommand(t, "vault", "--version"); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dir, "openvault"), "--version"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %q, want %q", *ran, want)
	}
}

// A110: with no vault, it says in one sentence how to install it, and fails.
func TestVault_MissingSaysHowToInstallIt(t *testing.T) {
	ran := onlyIn(t, t.TempDir())
	_, err := executeCommand(t, "vault", "collections")
	if exitCode(err) != ExitFailure || len(*ran) != 0 {
		t.Fatalf("exit %d (%v), ran %q", exitCode(err), err, *ran)
	}
	want := "PinkVault is not installed here; install it with " +
		"`brew trust --formula abyssinia-labs/tap/latem-vault`, then `brew install abyssinia-labs/tap/latem-vault`."
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// A113: the formula is pinkvault once the Homebrew tap has it, and
// latem-vault until then, read from the tap's checkout beside brew, whether
// brew is the repository's own (Apple silicon) or a link to it (Intel macOS,
// Linux).
func TestVaultFormula_PinkvaultOnceTheTapHasIt(t *testing.T) {
	if vaultFormula("") != "abyssinia-labs/tap/latem-vault" {
		t.Error("with no brew, want latem-vault")
	}
	prefix := t.TempDir()
	repo := filepath.Join(prefix, "Homebrew")
	brew := filepath.Join(repo, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil { // #nosec G306 -- a test's program
		t.Fatal(err)
	}
	link := filepath.Join(prefix, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(brew, link); err != nil {
		t.Fatal(err)
	}
	formulas := filepath.Join(repo, "Library", "Taps", "abyssinia-labs", "homebrew-tap", "Formula")
	if err := os.MkdirAll(formulas, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "latem-vault.rb"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{brew, link} {
		if got := vaultFormula(b); got != "abyssinia-labs/tap/latem-vault" {
			t.Errorf("%s, before 0.7.0: %q", b, got)
		}
	}
	if err := os.WriteFile(filepath.Join(formulas, "pinkvault.rb"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{brew, link} {
		if got := vaultFormula(b); got != "abyssinia-labs/tap/pinkvault" {
			t.Errorf("%s, after 0.7.0: %q", b, got)
		}
	}
}

// A110: the vault's own process is what runs: stdin reaches it, its stdout
// and stderr are the command's, and its exit status is latem's. This runs
// this test binary as latem, with a vault whose script exits 7.
func TestVault_ExecPassesStdioAndTheExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("latem vault is for macOS and Linux")
	}
	if os.Getenv("LATEM_TEST_VAULT_EXEC") == "1" {
		os.Args = []string{"latem", "vault", "collections", "--json"}
		os.Exit(run())
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nread line\necho \"out: $* $line\"\necho \"err: $(basename \"$0\")\" >&2\nexit 7\n"
	if err := os.WriteFile(filepath.Join(dir, "latem-vault"), []byte(script), 0o755); err != nil { // #nosec G306 -- a test's program
		t.Fatal(err)
	}
	home := t.TempDir()
	c := exec.Command(os.Args[0], "-test.run=^TestVault_ExecPassesStdioAndTheExitStatus$") // #nosec G204 -- this test binary
	c.Env = []string{"LATEM_TEST_VAULT_EXEC=1", "PATH=" + dir + ":/usr/bin:/bin", "HOME=" + home,
		"LATEM_CONFIG_DIR=" + filepath.Join(home, ".latem")}
	c.Stdin = strings.NewReader("from stdin\n")
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exit %v, want 7\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "out: collections --json from stdin\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := stderr.String(); got != "err: latem-vault\n" {
		t.Errorf("stderr = %q, want the vault's alone", got)
	}
}
