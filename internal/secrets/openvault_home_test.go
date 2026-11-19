package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A104: the vault's directory is found under its new name first and its old
// one after, so a machine reads it before and after the vault is migrated.
func TestVaultHome_TheNewNameFirstThenTheOld(t *testing.T) {
	home := t.TempDir()
	migrated := t.TempDir()
	if err := os.Mkdir(filepath.Join(migrated, ".latem-vault"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		environ []string
		want    string
	}{
		{"today's machine", []string{"HOME=" + home}, filepath.Join(home, ".openvault")},
		{"the old variable", []string{"HOME=" + home, "OPENVAULT_HOME=/srv/ov"}, "/srv/ov"},
		{"a migrated vault", []string{"HOME=" + migrated}, filepath.Join(migrated, ".latem-vault")},
		{"a migrated vault over the old variable", []string{"HOME=" + migrated, "OPENVAULT_HOME=/srv/ov"},
			filepath.Join(migrated, ".latem-vault")},
		{"the new variable over all", []string{"HOME=" + migrated, "OPENVAULT_HOME=/srv/ov", "LATEM_VAULT_HOME=/srv/lv"},
			"/srv/lv"},
		{"the new variable alone", []string{"LATEM_VAULT_HOME=/srv/lv"}, "/srv/lv"},
	} {
		got, err := VaultHome(tc.environ)
		if err != nil || got != tc.want {
			t.Errorf("%s: VaultHome = %q, %v; want %q", tc.name, got, err, tc.want)
		}
		socket, err := OpenVaultSocket(tc.environ)
		if err != nil || socket != filepath.Join(tc.want, OpenVaultSocketName) {
			t.Errorf("%s: OpenVaultSocket = %q, %v", tc.name, socket, err)
		}
	}
	if _, err := VaultHome([]string{"PATH=/bin"}); err == nil {
		t.Error("no HOME and no variable should be an error, not a vault in the working directory")
	}
}

// A113 (LAT-35): PinkVault's directory and variable come before both older
// names', so a machine reads the vault before, during and after its move to
// ~/.pinkvault.
func TestVaultHome_PinkVaultBeforeBothOlderNames(t *testing.T) {
	both := t.TempDir()
	for _, d := range []string{".latem-vault", ".openvault"} {
		if err := os.Mkdir(filepath.Join(both, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	moved := t.TempDir()
	for _, d := range []string{".pinkvault", ".latem-vault", ".openvault"} {
		if err := os.Mkdir(filepath.Join(moved, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pinkHome := filepath.Join(t.TempDir(), "pink")
	all := []string{"OPENVAULT_HOME=/srv/ov", "LATEM_VAULT_HOME=/srv/lv"}
	for _, tc := range []struct {
		name    string
		environ []string
		want    string
	}{
		{"not moved yet", []string{"HOME=" + both}, filepath.Join(both, ".latem-vault")},
		{"moved", []string{"HOME=" + moved}, filepath.Join(moved, ".pinkvault")},
		{"moved, over both older variables", append([]string{"HOME=" + moved}, all...), filepath.Join(moved, ".pinkvault")},
		{"not moved, the latem-vault variable", append([]string{"HOME=" + both}, all...), "/srv/lv"},
		{"the PinkVault variable over all", append([]string{"HOME=" + moved, "PINKVAULT_HOME=" + pinkHome}, all...), pinkHome},
		{"the PinkVault variable alone", []string{"PINKVAULT_HOME=" + pinkHome}, pinkHome},
		{"an empty PinkVault variable is not set", []string{"HOME=" + both, "PINKVAULT_HOME="}, filepath.Join(both, ".latem-vault")},
	} {
		got, err := VaultHome(tc.environ)
		if err != nil || got != tc.want {
			t.Errorf("%s: VaultHome = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// A113 (LAT-35): a vault in a test PINKVAULT_HOME is the one latem reads: it
// is here, and its socket is the one the provider dials.
func TestVaultHome_AVaultInATestPinkvaultHomeIsRead(t *testing.T) {
	home, pinkHome := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".latem-vault"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinkHome, "vault.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	environ := []string{"HOME=" + home, "PINKVAULT_HOME=" + pinkHome}
	if !OpenVaultHere(environ) {
		t.Error("the vault in PINKVAULT_HOME is not here")
	}
	if socket, err := OpenVaultSocket(environ); err != nil || socket != filepath.Join(pinkHome, OpenVaultSocketName) {
		t.Errorf("socket = %q, %v", socket, err)
	}
	if OpenVaultHere([]string{"HOME=" + home}) {
		t.Error("an empty ~/.latem-vault is a vault")
	}
}

// A104: ~/.latem-vault that is a file, not a directory, is not the vault.
func TestVaultHome_ANewNameThatIsNotADirectoryIsPassedOver(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".latem-vault"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := VaultHome([]string{"HOME=" + home}); got != filepath.Join(home, ".openvault") {
		t.Errorf("VaultHome = %q, want the old directory", got)
	}
}

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil { // #nosec G306 -- a test's program
		t.Fatal(err)
	}
	return p
}

// A104 (LAT-29): cargo installs the vault's command in ~/.cargo/bin, which is
// on no PATH an agent or an ssh shell gets; Homebrew's bin is not on a
// non-login shell's. Both are looked in after PATH.
func TestVaultSearchDirs_PathThenCargoThenHomebrew(t *testing.T) {
	got := VaultSearchDirs([]string{"PATH=/usr/bin:relative:/bin", "HOME=/home/m", "HOMEBREW_PREFIX=/brew"})
	want := []string{"/usr/bin", "/bin", "/home/m/.cargo/bin", "/brew/bin"}
	if !slices.Equal(got[:len(want)], want) {
		t.Errorf("dirs = %q, want %q first", got, want)
	}
	for _, d := range homebrewBins {
		if !slices.Contains(got, d) {
			t.Errorf("dirs = %q lack Homebrew's %s", got, d)
		}
	}
	if got := VaultSearchDirs([]string{"HOME=/home/m", "CARGO_HOME=/opt/cargo"}); got[0] != "/opt/cargo/bin" {
		t.Errorf("dirs = %q, want CARGO_HOME's bin in place of ~/.cargo's", got)
	}
}

// A104: latem-vault is found before openvault wherever each is, an openvault
// that only cargo's bin has is found, and a file that is not executable is
// not a command.
func TestFindVaultCommand_TheNewNameAnywhereThenTheOld(t *testing.T) {
	root := t.TempDir()
	path, cargo, brew := filepath.Join(root, "path"), filepath.Join(root, "cargo"), filepath.Join(root, "brew")
	dirs := []string{path, cargo, brew}
	if _, _, ok := FindVaultCommand(dirs); ok {
		t.Fatal("found a command in empty directories")
	}

	old := executable(t, cargo, OldVaultCommand)
	if name, p, ok := FindVaultCommand(dirs); !ok || name != OldVaultCommand || p != old {
		t.Errorf("FindVaultCommand = %q, %q, %v; want openvault in cargo's bin", name, p, ok)
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, LatemVaultCommand), nil, 0o644); err != nil { // #nosec G306 -- not executable, on purpose
		t.Fatal(err)
	}
	if name, _, _ := FindVaultCommand(dirs); name != OldVaultCommand {
		t.Errorf("found %q: a latem-vault that is not executable is not a command", name)
	}

	renamed := executable(t, brew, LatemVaultCommand)
	if name, p, ok := FindVaultCommand(dirs); !ok || name != LatemVaultCommand || p != renamed {
		t.Errorf("FindVaultCommand = %q, %q, %v; want latem-vault, though openvault is in an earlier directory", name, p, ok)
	}

	// A113: pinkvault before both, in the last directory looked in.
	pink := executable(t, brew+"-last", VaultCommand)
	dirs = append(dirs, filepath.Dir(pink))
	if name, p, ok := FindVaultCommand(dirs); !ok || name != VaultCommand || p != pink {
		t.Errorf("FindVaultCommand = %q, %q, %v; want pinkvault, though both older names are in earlier directories", name, p, ok)
	}
	for _, n := range []string{"pinkvault", "latem-vault", "openvault"} {
		if !IsVaultCommand(n) {
			t.Errorf("%s is not the vault's command", n)
		}
	}
	if IsVaultCommand("pink") || IsVaultCommand("lv") {
		t.Error("a short name is not looked for")
	}
}

func keyFile(t *testing.T, magic string, kind byte) string {
	t.Helper()
	dir := t.TempDir()
	body := append([]byte(magic), kind)
	body = append(body, make([]byte, 64)...)
	if err := os.WriteFile(filepath.Join(dir, "keys"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A104 (LAT-29): the service's passphrase note is said only for a key that
// has one. The key file's kind says so without opening the keys.
func TestVaultKeyPassphrase_ReadsTheKeyFilesKind(t *testing.T) {
	const magic = "openvault v1 keys\x00"
	for _, tc := range []struct {
		name              string
		dir               string
		passphrase, known bool
	}{
		{"a key file", keyFile(t, magic, 0x01), false, true},
		{"the Secure Enclave", keyFile(t, magic, 0x02), false, true},
		{"a passphrase", keyFile(t, magic, 0x03), true, true},
		{"the renamed vault's passphrase", keyFile(t, "latem-vault v1 keys\x00", 0x03), true, true},
		{"a kind not known yet", keyFile(t, magic, 0x09), false, false},
		{"not a key file", keyFile(t, "something else entirely\x00", 0x02), false, false},
		{"no key file", t.TempDir(), false, false},
	} {
		p, k := VaultKeyPassphrase(tc.dir)
		if p != tc.passphrase || k != tc.known {
			t.Errorf("%s: passphrase %v known %v, want %v %v", tc.name, p, k, tc.passphrase, tc.known)
		}
	}
	if p, k := NewOpenVaultProvider(filepath.Join(keyFile(t, magic, 0x03), OpenVaultSocketName), 0).KeyPassphrase(); !p || !k {
		t.Error("the provider does not read the key file beside its socket")
	}
}

// A104: what an error tells a person to type names the command this machine
// has, and is pinkvault when that is not known (A113).
func TestOpenVaultError_NamesTheCommandFound(t *testing.T) {
	socket := filepath.Join(t.TempDir(), OpenVaultSocketName)
	for _, tc := range []struct{ command, want string }{
		{"", "`pinkvault service install`"},
		{LatemVaultCommand, "`latem-vault service install`"},
		{OldVaultCommand, "`openvault service install`"},
	} {
		p := NewOpenVaultProvider(socket, 0).WithCommand(tc.command)
		_, err := p.Collections(context.Background())
		var ove *OpenVaultError
		if !errors.As(err, &ove) || !ove.NotRunning {
			t.Fatalf("err = %v, want a service that is not running", err)
		}
		if !strings.Contains(err.Error(), tc.want) || ove.ServiceFix() != strings.Trim(tc.want, "`") {
			t.Errorf("with command %q: %v, fix %q; want %s", tc.command, err, ove.ServiceFix(), tc.want)
		}
	}
	if NewOpenVaultProvider(socket, 0).Name() != "vault" {
		t.Error(`the store is named "vault" since A104`)
	}
}
