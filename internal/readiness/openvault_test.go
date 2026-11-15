package readiness

import (
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
)

// deployJob reads a secret and a variable in its environment.
func deployJob() Job {
	return Job{Ref: "tsedal deploy.yml web", Repo: "tsedal", Labels: []string{"linux"}, Runner: "linux",
		Report: needs.Report{Platform: "Linux", Kind: needs.KindLinux},
		Env:    "staging", Secrets: []string{"CLOUDFLARE_API_TOKEN"}, Vars: []string{"BASE_URL"}}
}

func vaultPlan(t *testing.T, v Vault, jobs ...Job) Kind {
	t.Helper()
	in := desktop()
	in.Jobs = jobs
	in.Vault = v
	return kind(t, Build(in), needs.KindLinux)
}

func ids(k Kind) []string {
	var out []string
	for _, it := range k.Items {
		out = append(out, it.ID)
	}
	return out
}

func person(it Item) bool { return len(it.Run) == 0 && len(it.Config) == 0 }

// A97: with no OpenVault, a secret is still set on this machine, and the
// item says where to read about keeping it in the vault instead. A variable
// is an item of its own.
func TestVaultItems_WithoutOpenVaultTheFixIsThisMachines(t *testing.T) {
	k := vaultPlan(t, Vault{}, deployJob())
	if got, want := ids(k), []string{"secrets-tsedal", "vars-tsedal"}; !slices.Equal(got, want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	s := item(t, k, "secrets-tsedal")
	if got := strings.Join(s.Yours[0], " "); got != "latem secrets set --repo tsedal --env staging CLOUDFLARE_API_TOKEN" {
		t.Errorf("secret's fix = %q", got)
	}
	if len(s.Manual) != 1 || !strings.Contains(s.Manual[0], `docs/secrets.md, "From OpenVault"`) {
		t.Errorf("manual = %q, want the way to OpenVault", s.Manual)
	}
	v := item(t, k, "vars-tsedal")
	if got := strings.Join(v.Yours[0], " "); got != "latem vars set --repo tsedal --env staging BASE_URL <value>" ||
		!slices.Equal(v.Vars, []string{"BASE_URL (staging)"}) || v.Soft {
		t.Errorf("variables item = %+v", v)
	}
	if k.State != Missing {
		t.Errorf("state %s, want missing", k.State)
	}
}

// A97: a machine that reads the vault, and holds the repository's
// collection, is told to set the name in the vault, as the person's.
func TestVaultItems_ANameTheCollectionLacksIsSetInTheVault(t *testing.T) {
	v := Vault{Installed: true, Here: true, On: true, Answers: true, Device: "Mini", Held: []string{"tsedal"}}
	k := vaultPlan(t, v, deployJob())
	s := item(t, k, "secrets-tsedal")
	if got := strings.Join(s.Yours[0], " "); got != "openvault set tsedal/staging/CLOUDFLARE_API_TOKEN --secret" {
		t.Errorf("secret's fix = %q", got)
	}
	if got := strings.Join(item(t, k, "vars-tsedal").Yours[0], " "); got != "openvault set tsedal/staging/BASE_URL --var" {
		t.Errorf("variable's fix = %q", got)
	}
	if !person(s) || !strings.Contains(s.Manual[0], "may write collection tsedal") {
		t.Errorf("item %+v, want the person's, on a device that may write the collection", s)
	}
}

// A97: a collection this device does not hold is one item, the person's on
// an admin device, naming this device as OpenVault knows it; no name can be
// said missing until it is held.
func TestVaultItems_ACollectionThisDeviceLacksIsScopedOnAnAdminDevice(t *testing.T) {
	v := Vault{Installed: true, Here: true, On: true, Answers: true, Device: "Mini", Held: []string{"other"}}
	k := vaultPlan(t, v, deployJob())
	if got := ids(k); !slices.Equal(got, []string{"openvault-collection-tsedal"}) {
		t.Fatalf("items %v, want the collection alone", got)
	}
	c := item(t, k, "openvault-collection-tsedal")
	if got := strings.Join(c.Yours[0], " "); got != "openvault scope Mini tsedal read" || !person(c) {
		t.Errorf("collection item = %+v", c)
	}
	if !slices.Equal(c.Secrets, []string{"CLOUDFLARE_API_TOKEN (staging)"}) || !slices.Equal(c.Vars, []string{"BASE_URL (staging)"}) {
		t.Errorf("names %v %v, want what waits on it", c.Secrets, c.Vars)
	}
}

// A97: a service that does not answer is latem's to start, and every name
// waits on it.
func TestVaultItems_AServiceThatDoesNotAnswerIsInstalled(t *testing.T) {
	v := Vault{Installed: true, Here: true, On: true}
	k := vaultPlan(t, v, deployJob())
	if got := ids(k); !slices.Equal(got, []string{"openvault-service"}) {
		t.Fatalf("items %v, want the service alone", got)
	}
	s := item(t, k, "openvault-service")
	if len(s.Run) != 1 || strings.Join(s.Run[0], " ") != "openvault service install" || s.Soft {
		t.Errorf("service item = %+v", s)
	}
	if !strings.Contains(strings.Join(s.Manual, " "), "passphrase") {
		t.Errorf("manual = %q, want what a key with a passphrase needs", s.Manual)
	}
}

// A97: a machine whose config reads a vault this device is not in is told
// to join it: the person's, here and on a device already in it.
func TestVaultItems_ADeviceNotInTheVaultJoinsIt(t *testing.T) {
	k := vaultPlan(t, Vault{Installed: true, On: true}, deployJob())
	if got := ids(k); !slices.Equal(got, []string{"openvault-join"}) {
		t.Fatalf("items %v, want join alone", got)
	}
	j := item(t, k, "openvault-join")
	if len(j.Yours) != 1 || strings.Join(j.Yours[0], " ") != "openvault join" || !person(j) || j.Soft ||
		!strings.Contains(j.Manual[0], "openvault approve") {
		t.Errorf("join item = %+v", j)
	}
}

// A97: a vault here, answering and holding one of the machine's
// repositories, that config.toml does not read yet: turning it on is a
// config item `setup apply` closes, and the names it holds are not missing.
func TestVaultItems_AVaultNotReadYetIsTurnedOn(t *testing.T) {
	v := Vault{Installed: true, Here: true, Answers: true, Device: "Mini", Held: []string{"tsedal"}}
	held := deployJob()
	held.Secrets, held.Vars, held.FromVault = nil, nil, true
	k := vaultPlan(t, v, held)
	if got := ids(k); !slices.Equal(got, []string{"openvault-on"}) {
		t.Fatalf("items %v, want openvault-on alone", got)
	}
	on := item(t, k, "openvault-on")
	if len(on.Config) != 1 || on.Config[0].What != ChangeOpenVault || on.Soft || len(on.Yours) > 0 {
		t.Errorf("item %+v, want a config change setup applies", on)
	}

	// What the vault lacks is set in it, once it is read.
	k = vaultPlan(t, v, deployJob())
	if got := ids(k); !slices.Equal(got, []string{"openvault-on", "secrets-tsedal", "vars-tsedal"}) {
		t.Fatalf("items %v", got)
	}
	if got := item(t, k, "secrets-tsedal").Yours[0][0]; got != "openvault" {
		t.Errorf("fix starts %q, want the vault's", got)
	}
}

// A97: a device with OpenVault installed and no vault is shown the way in,
// soft, beside this machine's own fix: it does not read the vault yet.
func TestVaultItems_InstalledAndNotJoinedIsAWayInBesideTheLocalFix(t *testing.T) {
	k := vaultPlan(t, Vault{Installed: true}, deployJob())
	if got := ids(k); !slices.Equal(got, []string{"openvault-join", "secrets-tsedal", "vars-tsedal"}) {
		t.Fatalf("items %v", got)
	}
	if !item(t, k, "openvault-join").Soft {
		t.Error("joining is one way of two here, and blocks nothing")
	}
	if got := item(t, k, "secrets-tsedal").Yours[0][0]; got != "latem" {
		t.Errorf("fix starts %q, want this machine's", got)
	}
}

// A97, done when: a machine that reads OpenVault and finds every name there
// plans with no secrets or variables item.
func TestVaultItems_EveryNameInTheVaultIsNoItem(t *testing.T) {
	v := Vault{Installed: true, Here: true, On: true, Answers: true, Device: "Mini", Held: []string{"tsedal"}}
	held := deployJob()
	held.Secrets, held.Vars, held.FromVault = nil, nil, true
	k := vaultPlan(t, v, held)
	if len(k.Items) != 0 || k.State != Ready {
		t.Errorf("kind %s with items %v, want ready with none", k.State, ids(k))
	}
}

// A97: a variable's name leaves the machine as a secret's does, and never
// its value.
func TestAdvert_VariablesAreNamed(t *testing.T) {
	in := desktop()
	in.Jobs = []Job{deployJob()}
	r := Advert(Build(in))
	for _, k := range r.Kinds {
		for _, it := range k.Items {
			if it.Title == "tsedal's variables set here" {
				if it.Have != "BASE_URL (staging) not set" {
					t.Errorf("have = %q", it.Have)
				}
				return
			}
		}
	}
	t.Fatalf("no variables item in %+v", r.Kinds)
}
