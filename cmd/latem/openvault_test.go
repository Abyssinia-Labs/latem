package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// fakeOpenVault is OpenVault as the commands see it: which collections this
// machine holds, and the scoped names in each.
type fakeOpenVault struct {
	names map[string][]string // collection -> scoped names, as secrets.OpenVaultProvider.Names gives them
	err   error
}

func (f *fakeOpenVault) Collections(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for c := range f.names {
		out = append(out, c)
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeOpenVault) Names(_ context.Context, repos []string) (names, absent []string, err error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	for _, r := range slices.Sorted(slices.Values(repos)) {
		held, ok := f.names[r]
		if !ok {
			absent = append(absent, r)
		}
		names = append(names, held...)
	}
	slices.Sort(names)
	return names, absent, nil
}

func (f *fakeOpenVault) Socket() string { return "/home/tester/.openvault/service.sock" }

var stoppedOpenVault = &secrets.OpenVaultError{
	Op: "collections", NotRunning: true, Socket: "/home/tester/.openvault/service.sock",
}

func openVaultDoctor(t *testing.T, ov *fakeOpenVault, enabled bool) *doctor {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.Repos["latem-cloud"] = &config.Repo{URL: "https://example.invalid/latem-cloud.git", AllowedRefs: []string{"main"}}
	cfg.Repos["wake"] = &config.Repo{URL: "https://example.invalid/wake.git", AllowedRefs: []string{"main"}}
	cfg.Secrets.OpenVault = enabled
	d := newTestDoctor("linux", dir, nil, cfg, nil)
	d.openVault = func() (openVaultCollections, error) { return ov, nil }
	return d
}

func TestDoctorOpenVault_SaysNothingUnlessConfigReadsIt(t *testing.T) {
	d := openVaultDoctor(t, &fakeOpenVault{err: stoppedOpenVault}, false)
	if got := d.checkOpenVault(context.Background()); len(got) != 0 {
		t.Errorf("checks = %+v; a machine that does not read OpenVault has nothing to check", got)
	}
}

func TestDoctorOpenVault_AStoppedServiceFailsWithTheCommandThatStartsIt(t *testing.T) {
	got := openVaultDoctor(t, &fakeOpenVault{err: stoppedOpenVault}, true).checkOpenVault(context.Background())
	c := checkByID(got, "openvault.service")
	if c == nil || c.Level != levelFail {
		t.Fatalf("checks = %+v, want openvault.service failing", got)
	}
	if !slices.Contains(c.Fix, "openvault service install") {
		t.Errorf("fix = %q, want the command that starts it", c.Fix)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "service.sock") {
		t.Errorf("notes = %q, want the socket it asked, which only this terminal sees", c.Notes)
	}
}

// "Machine X lacks collection Y", by name: the repositories whose secrets
// this machine cannot read from OpenVault.
func TestDoctorOpenVault_NamesEachRepositoryThisMachineHasNoCollectionFor(t *testing.T) {
	ov := &fakeOpenVault{names: map[string][]string{"latem-cloud": nil, "ssh": nil}}
	got := openVaultDoctor(t, ov, true).checkOpenVault(context.Background())

	if c := checkByID(got, "openvault.service"); c == nil || c.Level != levelPass {
		t.Errorf("service check = %+v, want a pass", c)
	}
	if c := checkByID(got, "openvault.collections"); c == nil || c.Detail != "latem-cloud" {
		t.Errorf("collections check = %+v, want latem-cloud alone: ssh is not a repository here", c)
	}
	c := checkByID(got, "openvault.collections.absent")
	if c == nil || c.Level != levelWarn {
		t.Fatalf("checks = %+v, want a warning for the absent collections", got)
	}
	if !strings.Contains(c.Detail, "no collection app, wake") {
		t.Errorf("detail = %q, want app and wake named", c.Detail)
	}
	if len(c.Fix) == 0 || !strings.Contains(c.Fix[0], "openvault scope") {
		t.Errorf("fix = %q", c.Fix)
	}
}

func withOpenVaultStores(t *testing.T, ov openVaultLister) *memoryKeychain {
	t.Helper()
	kc := &memoryKeychain{items: map[string]string{}}
	prev := newSecretStores
	newSecretStores = func(p config.Paths) (secretStores, error) {
		return secretStores{file: secrets.NewFileStore(p.SecretsEnv), keychain: kc, openvault: ov}, nil
	}
	t.Cleanup(func() { newSecretStores = prev })
	return kc
}

func TestSecretsList_ListsOpenVaultFirstAndNamesARepositoryWithNoCollection(t *testing.T) {
	dir := configDirWith(t, secretsConfig+"\n[secrets]\nopenvault = true\n")
	kc := withOpenVaultStores(t, &fakeOpenVault{names: map[string][]string{
		"latem-cloud": {secrets.ScopedName("CONVEX_DEPLOY_KEY", "latem-cloud", "production")},
	}})
	kc.items[secrets.ScopedName("CONVEX_DEPLOY_KEY", "latem-cloud", "production")] = "x"

	out, err := latemIn(t, dir, "", "secrets", "list", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "latem-cloud / production\tCONVEX_DEPLOY_KEY\topenvault, keychain (openvault answers)") {
		t.Errorf("secrets list does not say OpenVault answers first:\n%s", out)
	}
	if !strings.Contains(out, "this machine has no collection app") {
		t.Errorf("secrets list does not name the repository with no collection:\n%s", out)
	}
}

func TestSecretsList_AStoppedOpenVaultIsSaidAndTheOtherStoresStillList(t *testing.T) {
	dir := configDirWith(t, secretsConfig+"\n[secrets]\nopenvault = true\n")
	kc := withOpenVaultStores(t, &fakeOpenVault{err: stoppedOpenVault})
	kc.items["TOKEN"] = "x"

	out, err := latemIn(t, dir, "", "secrets", "list", "--no-color")
	if err != nil {
		t.Fatalf("secrets list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "openvault service") || !strings.Contains(out, "*\tTOKEN\tkeychain") {
		t.Errorf("secrets list =\n%s\nwant the stopped service said and the Keychain's names listed", out)
	}
}

func TestSecretsList_OpenVaultIsNotAskedUnlessConfigReadsIt(t *testing.T) {
	dir := configDirWith(t, secretsConfig)
	withOpenVaultStores(t, &fakeOpenVault{err: stoppedOpenVault})
	out, err := latemIn(t, dir, "", "secrets", "list", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "openvault") {
		t.Errorf("OpenVault was asked on a machine that does not read it:\n%s", out)
	}
}

func TestSecretsSet_WarnsWhenOpenVaultHoldsTheNameAndIsReadFirst(t *testing.T) {
	dir := configDirWith(t, secretsConfig+"\n[secrets]\nopenvault = true\n")
	withOpenVaultStores(t, &fakeOpenVault{names: map[string][]string{
		"latem-cloud": {secrets.ScopedName("CONVEX_DEPLOY_KEY", "latem-cloud", "production")},
	}})
	out, err := latemIn(t, dir, "dk_prod_0123456789\n", "secrets", "set",
		"--repo", "latem-cloud", "--env", "production", "CONVEX_DEPLOY_KEY")
	if err != nil {
		t.Fatalf("secrets set: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OpenVault holds latem-cloud/production/CONVEX_DEPLOY_KEY, and is read first") {
		t.Errorf("no warning that OpenVault answers first:\n%s", out)
	}
}

type countingLister struct {
	openVaultLister
	calls int
}

func (c *countingLister) Names(ctx context.Context, repos []string) ([]string, []string, error) {
	c.calls++
	return c.openVaultLister.Names(ctx, repos)
}

// A readiness beat reads the inventory once per repository; OpenVault is
// asked once.
func TestOnceOpenVault_AsksOncePerSetOfRepositories(t *testing.T) {
	inner := &countingLister{openVaultLister: &fakeOpenVault{names: map[string][]string{"app": {"app__TOKEN"}}}}
	o := &onceOpenVault{inner: inner}
	for range 5 {
		names, _, err := o.Names(context.Background(), []string{"wake", "app"})
		if err != nil || !slices.Equal(names, []string{"app__TOKEN"}) {
			t.Fatalf("Names = %v, %v", names, err)
		}
	}
	if _, _, err := o.Names(context.Background(), []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Errorf("asked %d times, want once per set of repositories", inner.calls)
	}
}
