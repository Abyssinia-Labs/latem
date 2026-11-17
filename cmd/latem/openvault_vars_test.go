package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

const openVaultConfig = baseConfig + "\n[secrets]\nopenvault = true\n"

// writeConfig replaces dir's config.toml.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A96: latem vars list shows OpenVault's variables beside config.toml's, and
// which a job reads where both hold one.
func TestVarsList_ShowsOpenVaultsVariablesAndWhichAnswers(t *testing.T) {
	dir := configDirWith(t, openVaultConfig)
	noAgentRunning(t)
	withOpenVaultStores(t, &fakeOpenVault{vars: map[string]secrets.Variables{"app": {
		"":           {"LATEM": "true"},
		"production": {"REGION": "eu"},
	}}})
	if out, err := latemIn(t, dir, "", "vars", "set", "--repo", "app", "LATEM", "false"); err != nil {
		t.Fatalf("vars set: %v\n%s", err, out)
	}

	out, err := latemIn(t, dir, "", "vars", "list", "--no-color")
	if err != nil {
		t.Fatalf("vars list: %v\n%s", err, out)
	}
	for _, want := range []string{
		"REPO\tENVIRONMENT\tNAME\tVALUE\tSOURCE",
		"app\t-\tLATEM\ttrue\tvault, config.toml (vault answers)",
		"app\tproduction\tREGION\teu\tvault",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("vars list lacks %q:\n%s", want, out)
		}
	}
}

// A96: a stopped service is said, and config.toml's variables still list.
func TestVarsList_AStoppedOpenVaultIsSaidAndConfigsStillList(t *testing.T) {
	dir := configDirWith(t, openVaultConfig)
	noAgentRunning(t)
	withOpenVaultStores(t, &fakeOpenVault{err: stoppedOpenVault})
	if out, err := latemIn(t, dir, "", "vars", "set", "--repo", "app", "LATEM", "false"); err != nil {
		t.Fatalf("vars set: %v\n%s", err, out)
	}

	out, err := latemIn(t, dir, "", "vars", "list", "--no-color")
	if err != nil {
		t.Fatalf("vars list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "latem-vault service") || !strings.Contains(out, "app\t-\tLATEM\tfalse\tconfig.toml") {
		t.Errorf("vars list =\n%s\nwant the stopped service said and config.toml's listed", out)
	}
}

// A96: a value set in config.toml that OpenVault shadows is said to be, at
// once, rather than found later when a job reads the other one.
func TestVarsSet_WarnsWhenOpenVaultHoldsTheSameVariable(t *testing.T) {
	dir := configDirWith(t, openVaultConfig)
	noAgentRunning(t)
	withOpenVaultStores(t, &fakeOpenVault{vars: map[string]secrets.Variables{"app": {"production": {"REGION": "eu"}}}})

	out, err := latemIn(t, dir, "", "vars", "set", "--repo", "app", "--env", "production", "REGION", "us")
	if err != nil {
		t.Fatalf("vars set: %v\n%s", err, out)
	}
	if !strings.Contains(out, "the vault holds app/production/REGION") ||
		!strings.Contains(out, "latem-vault set app/production/REGION --var") {
		t.Errorf("vars set =\n%s\nwant a warning naming the vault's entry", out)
	}
	out, err = latemIn(t, dir, "", "vars", "set", "--repo", "app", "REGION", "us")
	if err != nil {
		t.Fatalf("vars set: %v\n%s", err, out)
	}
	if strings.Contains(out, "the vault holds") {
		t.Errorf("vars set =\n%s\nwarned for the repository's own scope, which the vault does not hold", out)
	}
}

// A96: workflows check counts OpenVault's variables, so one only the vault
// holds is not reported as reading empty.
func TestWorkflowsCheck_AVariableOnlyOpenVaultHoldsIsNotMissing(t *testing.T) {
	dir, repo, _ := checkFixture(t)
	writeConfig(t, dir, openVaultConfig)
	withOpenVaultStores(t, &fakeOpenVault{vars: map[string]secrets.Variables{"app": {"": {"REGION": "eu"}}}})

	out, _ := latemIn(t, dir, "", "workflows", "check", repo, "--no-color")
	if strings.Contains(out, "vars with no value here") {
		t.Errorf("a variable OpenVault holds is reported missing:\n%s", out)
	}
}

// A96: a stopped service is said where a workflow reads vars, with the
// command that starts it.
func TestWorkflowsCheck_AStoppedOpenVaultIsSaidWhereVarsAreRead(t *testing.T) {
	dir, repo, _ := checkFixture(t)
	writeConfig(t, dir, openVaultConfig)
	withOpenVaultStores(t, &fakeOpenVault{err: stoppedOpenVault})

	out, _ := latemIn(t, dir, "", "workflows", "check", repo, "--no-color")
	for _, want := range []string{"a job here that reads vars fails", "latem-vault service install"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
