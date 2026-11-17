package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// deployRepo reads a secret and a variable in its staging environment.
const deployRepo = `on: push
jobs:
  deploy:
    runs-on: macos-latest
    environment: staging
    steps:
      - run: ./deploy.sh
        env:
          TOKEN: ${{ secrets.TOKEN }}
          BASE: ${{ vars.BASE_URL }}
`

// vaultMachine is a Mac with OpenVault installed and a vault on it, whose
// service answers as ov does, and app's deploy job.
func vaultMachine(t *testing.T, ov *fakeOpenVault) (string, *setupRecorder) {
	t.Helper()
	dir, rec := setupMachine(t, fstest.MapFS{".github/workflows/deploy.yml": {Data: []byte(deployRepo)}})
	delete(rec.machine.notInstalled, "openvault")
	rec.machine.vault, rec.machine.vaultHere = ov, true
	noAgentRunning(t)
	return dir, rec
}

func setupPlanIn(t *testing.T, dir string) readiness.Plan {
	t.Helper()
	out, err := latemIn(t, dir, "", "setup", "--json")
	if err != nil {
		t.Fatalf("setup --json: %v\n%s", err, out)
	}
	var plan readiness.Plan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return plan
}

// A97: a vault here that holds the repository, which config.toml does not
// read yet, plans one item, latem's: turning it on. `setup apply` writes it,
// and the next plan has no secrets or variables item.
func TestSetupOpenVault_AVaultNotReadYetIsTurnedOnByApply(t *testing.T) {
	ov := &fakeOpenVault{device: "Mini",
		names: map[string][]string{"app": {secrets.ScopedName("TOKEN", "app", "staging")}},
		vars:  map[string]secrets.Variables{"app": {"staging": {"BASE_URL": "https://staging.example.invalid"}}}}
	dir, _ := vaultMachine(t, ov)

	k := kindIn(t, setupPlanIn(t, dir), "macos")
	if got := itemIDs(k); !slices.Equal(got, []string{"vault-on"}) {
		t.Fatalf("items %v, want openvault-on alone: the vault holds both names", got)
	}

	_, stderr, err := applyIn(t, dir, "vault-on")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "vault-on: wrote [secrets] vault = true; closed") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if !loadConfigIn(t, dir).Secrets.Vault {
		t.Errorf("config.toml does not read OpenVault:\n%s", readConfigFile(t, dir))
	}
	k = kindIn(t, setupPlanIn(t, dir), "macos")
	if slices.ContainsFunc(itemIDs(k), func(id string) bool {
		return strings.HasPrefix(id, "secrets-") || strings.HasPrefix(id, "vars-") || strings.HasPrefix(id, "vault")
	}) {
		t.Errorf("items %v, want none of the vault's or the names'", itemIDs(k))
	}
}

// A97: what is the person's to do in the vault is refused with exit 4, and
// says what they would type and where: a name the collection lacks, set on a
// device that may write it.
func TestSetupOpenVault_ANameTheVaultLacksIsThePersons(t *testing.T) {
	ov := &fakeOpenVault{device: "Mini", names: map[string][]string{"app": nil}}
	dir, _ := vaultMachine(t, ov)
	writeConfig(t, dir, readConfigFile(t, dir)+"\n[secrets]\nopenvault = true\n")

	k := kindIn(t, setupPlanIn(t, dir), "macos")
	if got := itemIDs(k); !slices.Equal(got, []string{"secrets-app", "vars-app"}) {
		t.Fatalf("items %v", got)
	}
	_, stderr, err := applyIn(t, dir, "secrets-app", "vars-app")
	if exitCode(err) != ExitNoTerminal {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	if strings.Contains(stderr, "on this machine;") {
		t.Errorf("stderr says the vault's commands are typed here:\n%s", stderr)
	}
	for _, want := range []string{
		"  openvault set app/staging/TOKEN --secret",
		"  openvault set app/staging/BASE_URL --var",
		"may write collection app",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// A97: a collection this device does not hold is scoped on an admin device,
// naming this one as OpenVault does. The person's, exit 4.
func TestSetupOpenVault_ACollectionThisDeviceLacksIsScopedByThePerson(t *testing.T) {
	ov := &fakeOpenVault{device: "Mini", names: map[string][]string{"other": nil}}
	dir, _ := vaultMachine(t, ov)
	writeConfig(t, dir, readConfigFile(t, dir)+"\n[secrets]\nopenvault = true\n")

	k := kindIn(t, setupPlanIn(t, dir), "macos")
	if got := itemIDs(k); !slices.Equal(got, []string{"vault-collection-app"}) {
		t.Fatalf("items %v", got)
	}
	_, stderr, err := applyIn(t, dir, "vault-collection-app")
	if exitCode(err) != ExitNoTerminal || !strings.Contains(stderr, "  openvault scope Mini app read") {
		t.Errorf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
}

// A97: a service that does not answer is latem's to install, and once it
// answers, what it holds closes the rest.
func TestSetupOpenVault_AServiceThatDoesNotAnswerIsInstalledByApply(t *testing.T) {
	ov := &fakeOpenVault{device: "Mini", err: stoppedOpenVault,
		names: map[string][]string{"app": {secrets.ScopedName("TOKEN", "app", "staging")}},
		vars:  map[string]secrets.Variables{"app": {"staging": {"BASE_URL": "https://staging.example.invalid"}}}}
	dir, rec := vaultMachine(t, ov)
	writeConfig(t, dir, readConfigFile(t, dir)+"\n[secrets]\nopenvault = true\n")
	rec.machine.gives["openvault service install"] = [2]string{"openvault", "/opt/homebrew/bin"}
	rec.machine.then = map[string]func(){"openvault service install": func() { ov.err = nil }}

	k := kindIn(t, setupPlanIn(t, dir), "macos")
	if got := itemIDs(k); !slices.Equal(got, []string{"vault-service"}) {
		t.Fatalf("items %v, want the service alone", got)
	}
	_, stderr, err := applyIn(t, dir, "vault-service")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !slices.Equal(rec.machine.installs, []string{"openvault service install"}) {
		t.Errorf("ran %q", rec.machine.installs)
	}
	if got := itemIDs(kindIn(t, setupPlanIn(t, dir), "macos")); len(got) != 0 {
		t.Errorf("items %v after the service answers, want none", got)
	}
}

// A97: a machine that reads OpenVault, on a device with no vault, is told to
// join it: the person's.
func TestSetupOpenVault_NoVaultOnThisDeviceIsJoinedByThePerson(t *testing.T) {
	dir, rec := vaultMachine(t, &fakeOpenVault{err: stoppedOpenVault})
	rec.machine.vaultHere = false
	writeConfig(t, dir, readConfigFile(t, dir)+"\n[secrets]\nopenvault = true\n")

	_, stderr, err := applyIn(t, dir, "vault-join")
	if exitCode(err) != ExitNoTerminal || !strings.Contains(stderr, "  openvault join") ||
		!strings.Contains(stderr, "openvault approve") {
		t.Errorf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
}

// A97: without OpenVault, a secret is still `latem secrets set`, with the way
// to the vault said, and a variable no store holds is an item of its own.
func TestSetupOpenVault_WithoutItTheFixIsThisMachines(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/deploy.yml": {Data: []byte(deployRepo)}})
	noAgentRunning(t)
	k := kindIn(t, setupPlanIn(t, dir), "macos")
	if got := itemIDs(k); !slices.Equal(got, []string{"secrets-app", "vars-app"}) {
		t.Fatalf("items %v", got)
	}
	_, stderr, err := applyIn(t, dir, "secrets-app", "vars-app")
	if exitCode(err) != ExitNoTerminal {
		t.Fatalf("exit %d (%v)\n%s", exitCode(err), err, stderr)
	}
	for _, want := range []string{
		"  latem secrets set --repo app --env staging TOKEN",
		`  latem vars set --repo app --env staging BASE_URL "<value>"`,
		`docs/secrets.md, "From the vault"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// A104 (LAT-29): `setup apply` runs the service's install itself, by the
// command found here, and takes the item's id before the rename for one
// release. A key known to open alone is not told what a passphrase needs.
func TestSetupOpenVault_ApplyTakesTheOldIDAndRunsTheInstallItself(t *testing.T) {
	ov := &fakeOpenVault{device: "Mini", err: stoppedOpenVault, keyKnown: true,
		names: map[string][]string{"app": {secrets.ScopedName("TOKEN", "app", "staging")}},
		vars:  map[string]secrets.Variables{"app": {"staging": {"BASE_URL": "https://staging.example.invalid"}}}}
	dir, rec := vaultMachine(t, ov)
	writeConfig(t, dir, readConfigFile(t, dir)+"\n[secrets]\nvault = true\n")
	rec.machine.gives["openvault service install"] = [2]string{"openvault", "/opt/homebrew/bin"}
	rec.machine.then = map[string]func(){"openvault service install": func() { ov.err = nil }}

	s := itemIn(t, kindIn(t, setupPlanIn(t, dir), "macos"), "vault-service")
	if len(s.Manual) != 0 {
		t.Errorf("manual = %q for a key in the Secure Enclave", s.Manual)
	}
	_, stderr, err := applyIn(t, dir, "openvault-service")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !slices.Equal(rec.machine.installs, []string{"openvault service install"}) {
		t.Errorf("ran %q, want the install by the command this machine has", rec.machine.installs)
	}
	if !strings.Contains(stderr, "vault-service: ran openvault service install; closed") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// A104: latem-vault is the command a machine that has both is told to type.
func TestSetupOpenVault_TheNewCommandBeforeTheOld(t *testing.T) {
	ov := &fakeOpenVault{device: "Mini", names: map[string][]string{"app": {}}}
	dir, rec := vaultMachine(t, ov)
	delete(rec.machine.notInstalled, "latem-vault")
	writeConfig(t, dir, readConfigFile(t, dir)+"\n[secrets]\nvault = true\n")

	s := itemIn(t, kindIn(t, setupPlanIn(t, dir), "macos"), "secrets-app")
	if got := strings.Join(s.Yours[0], " "); got != "latem-vault set app/staging/TOKEN --secret" {
		t.Errorf("fix = %q, want latem-vault's", got)
	}
}

func itemIn(t *testing.T, k readiness.Kind, id string) readiness.Item {
	t.Helper()
	for _, it := range k.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %s in %v", id, itemIDs(k))
	return readiness.Item{}
}
