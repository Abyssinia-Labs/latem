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

// A110: `latem vault …` runs latem-vault with the same arguments, flags
// included, and takes none of them for itself.
func TestVault_RunsLatemVaultWithEveryArgument(t *testing.T) {
	dir := fakeVaultDir(t, "latem-vault", "openvault")
	ran := onlyIn(t, t.TempDir(), dir)
	if _, err := executeCommand(t, "vault", "collections", "--json", "--help"); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dir, "latem-vault"), "collections", "--json", "--help"}; !slices.Equal(*ran, want) {
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
	want := "latem vault is not installed here; install it with " +
		"`brew trust --formula abyssinia-labs/tap/latem-vault`, then `brew install abyssinia-labs/tap/latem-vault`."
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
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
