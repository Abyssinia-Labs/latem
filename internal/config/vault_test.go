package config

import (
	"path/filepath"
	"testing"
)

// A104: `[secrets] vault = true` reads the vault, and so does the key's name
// before latem vault was renamed, `openvault = true`, which a machine's
// config.toml says until someone changes it.
func TestSecretsVault_EitherNameTurnsItOn(t *testing.T) {
	opts := Options{Home: "/nonexistent-home", Getenv: func(string) string { return "" }, LookupIP: testLookupIP}
	for _, tc := range []struct {
		toml string
		want bool
	}{
		{"", false},
		{"\n[secrets]\nvault = true\n", true},
		{"\n[secrets]\nopenvault = true\n", true},
		{"\n[secrets]\nvault = false\nopenvault = true\n", true},
		{"\n[secrets]\nvault = false\n", false},
	} {
		cfg, err := Parse([]byte(minimalValid+tc.toml), opts)
		if err != nil {
			t.Fatalf("%q: %v", tc.toml, err)
		}
		if cfg.Secrets.Vault != tc.want {
			t.Errorf("%q: Vault = %v, want %v", tc.toml, cfg.Secrets.Vault, tc.want)
		}
	}
}

// A104: the variables' digests are kept as vault-vars.json, and the name it
// had before is known so it can be read once.
func TestPaths_VarDigestsUnderTheNewNameAndTheOld(t *testing.T) {
	p := DerivePaths("/c")
	if p.VarDigests != filepath.Join("/c", "vault-vars.json") || p.OldVarDigests != filepath.Join("/c", "openvault-vars.json") {
		t.Errorf("VarDigests %q, OldVarDigests %q", p.VarDigests, p.OldVarDigests)
	}
}
