package secrets

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OpenVault is being renamed latem vault (A104). Its command, its directory
// and the variable that moves the directory all change name, and a machine
// moves when its person migrates it, not when latem is updated. Until every
// machine has moved, latem finds the vault under either name, the new one
// first.

// Vault command names, the new first.
const (
	VaultCommand    = "latem-vault"
	OldVaultCommand = "openvault"
)

// VaultCommands are the names the vault's command is looked for by, in order.
var VaultCommands = []string{VaultCommand, OldVaultCommand}

// IsVaultCommand reports whether name is the vault's command, by either name.
func IsVaultCommand(name string) bool {
	return name == VaultCommand || name == OldVaultCommand
}

// VaultHome is the vault's directory for environ, which is os.Environ's form:
//
//	$LATEM_VAULT_HOME      when it is set
//	$HOME/.latem-vault     when it exists
//	$OPENVAULT_HOME        when it is set
//	$HOME/.openvault       otherwise
//
// A variable is taken as it is, as the vault takes it. The new default
// directory is taken only once it exists, because until the vault is
// migrated it does not, and the old one is where the vault still is.
func VaultHome(environ []string) (string, error) {
	var home, newHome, oldHome string
	for _, kv := range environ {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "HOME":
			home = v
		case "LATEM_VAULT_HOME":
			newHome = v
		case "OPENVAULT_HOME":
			oldHome = v
		}
	}
	if newHome != "" {
		return newHome, nil
	}
	if home != "" {
		if fi, err := os.Stat(filepath.Join(home, ".latem-vault")); err == nil && fi.IsDir() {
			return filepath.Join(home, ".latem-vault"), nil
		}
	}
	if oldHome != "" {
		return oldHome, nil
	}
	if home != "" {
		return filepath.Join(home, ".openvault"), nil
	}
	return "", errors.New("secrets: vault: none of LATEM_VAULT_HOME, OPENVAULT_HOME or HOME is set, so there is no socket to read")
}

// VaultSearchDirs are where the vault's command is looked for, in order:
// environ's PATH, then cargo's bin, then Homebrew's. `cargo install` puts it
// in ~/.cargo/bin, which is on no PATH a service manager or an ssh command
// gives, and Homebrew's bin is not on a non-login ssh shell's.
func VaultSearchDirs(environ []string) []string {
	var path, home, cargo, brew string
	for _, kv := range environ {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "PATH":
			path = v
		case "HOME":
			home = v
		case "CARGO_HOME":
			cargo = v
		case "HOMEBREW_PREFIX":
			brew = v
		}
	}
	var dirs []string
	add := func(d string) {
		if d != "" && filepath.IsAbs(d) {
			for _, have := range dirs {
				if have == d {
					return
				}
			}
			dirs = append(dirs, d)
		}
	}
	for _, d := range filepath.SplitList(path) {
		add(d)
	}
	switch {
	case cargo != "":
		add(filepath.Join(cargo, "bin"))
	case home != "":
		add(filepath.Join(home, ".cargo", "bin"))
	}
	if brew != "" {
		add(filepath.Join(brew, "bin"))
	}
	for _, d := range homebrewBins {
		add(d)
	}
	return dirs
}

// homebrewBins are Homebrew's default prefixes' bin: Apple silicon, Intel
// macOS, and Linux.
var homebrewBins = []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"}

// LookVaultCommand finds the program name, which is one of [VaultCommands],
// in dirs: the first regular, executable file of that name.
func LookVaultCommand(name string, dirs []string) (string, error) {
	for _, d := range dirs {
		c := filepath.Join(d, name)
		// #nosec G703 -- name is one of two fixed names and dirs are PATH,
		// cargo's and Homebrew's bins.
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", &os.PathError{Op: "look", Path: name, Err: os.ErrNotExist}
}

// FindVaultCommand is the vault's command in dirs: latem-vault in any of
// them, else openvault. ok is false when there is neither.
func FindVaultCommand(dirs []string) (name, path string, ok bool) {
	for _, n := range VaultCommands {
		if p, err := LookVaultCommand(n, dirs); err == nil {
			return n, p, true
		}
	}
	return "", "", false
}

// VaultCommandHere is the name of the vault's command on this machine, for
// what latem tells a person to type: the one [FindVaultCommand] finds from
// this process's environment, else latem-vault. It is a name and never a
// path, so a message that names it can leave the machine.
func VaultCommandHere() string {
	if name, _, ok := FindVaultCommand(VaultSearchDirs(os.Environ())); ok {
		return name
	}
	return VaultCommand
}

// The key file's header, then a byte that says how the keys are kept
// (OpenVault keystore.md, key file).
var vaultKeyMagics = [][]byte{[]byte("openvault v1 keys\x00"), []byte("latem-vault v1 keys\x00")}

const (
	vaultKeySoftware      = 0x01
	vaultKeySecureEnclave = 0x02
	vaultKeyPassphrase    = 0x03
)

// VaultKeyPassphrase reports whether the key file in the vault directory dir
// opens with a passphrase, which a service started at login has nobody to
// type. It reads the file's header and kind and nothing after them, so no
// key is read. known is false when the file cannot be read or is a kind this
// does not know.
func VaultKeyPassphrase(dir string) (passphrase, known bool) {
	f, err := os.Open(filepath.Join(dir, "keys")) // #nosec G304 -- the vault's own key file
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()
	longest := 0
	for _, m := range vaultKeyMagics {
		longest = max(longest, len(m))
	}
	head := make([]byte, longest+1)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	for _, m := range vaultKeyMagics {
		if len(head) <= len(m) || string(head[:len(m)]) != string(m) {
			continue
		}
		switch head[len(m)] {
		case vaultKeyPassphrase:
			return true, true
		case vaultKeySoftware, vaultKeySecureEnclave:
			return false, true
		}
		return false, false
	}
	return false, false
}
