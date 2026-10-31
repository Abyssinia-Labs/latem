package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/appstore"
	"github.com/Abyssinia-Labs/latem/internal/playstore"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

const (
	storeTestBundle = "com.example.app"
	storeTestStore  = "https://github.com/example/signing.git"
	storeTestBranch = "team-EXAMPLE123"
	storeTestAlias  = "alias-sentinel-value"
)

var storeTestNow = time.Unix(1_800_000_000, 0).UTC()

func inDays(n int) string {
	return storeTestNow.Add(time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

// fakeAppStores stands in for Apple and Google, and writes down everything asked
// of them: what a check is allowed to ask is the point of half these tests.
type fakeAppStores struct {
	mu       sync.Mutex
	requests []string

	certType   string
	certName   string
	certDays   int
	profiles   []map[string]any
	buildState string
	buildDays  int
	noApp      bool
	noBundle   bool
	noBuild    bool

	appleStatus int
	appleBody   string

	tokenStatus int
	tokenBody   string
	playStatus  int
	playBody    string
}

func healthyStores() *fakeAppStores {
	return &fakeAppStores{
		certType: "DISTRIBUTION", certName: "Apple Distribution: Example", certDays: 300,
		buildState: "VALID", buildDays: 80,
		profiles: []map[string]any{storeProfile("p1", "match AppStore "+storeTestBundle, "IOS_APP_STORE", "ACTIVE", 300)},
		playBody: `{"releases":[{"releaseName":"401","releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_PUBLISHED",
			"activeArtifacts":[{"versionCode":401}]}]}`,
	}
}

func storeProfile(id, name, kind, state string, days int) map[string]any {
	return map[string]any{
		"type": "profiles", "id": id,
		"attributes": map[string]any{
			"name": name, "profileType": kind, "profileState": state, "expirationDate": inDays(days),
		},
	}
}

func (f *fakeAppStores) note(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
}

func (f *fakeAppStores) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeAppStores) apple(w http.ResponseWriter, r *http.Request) {
	f.note(r)
	if f.appleStatus != 0 && r.URL.Query().Get("filter[bundleId]") != "" {
		w.WriteHeader(f.appleStatus)
		_, _ = w.Write([]byte(f.appleBody))
		return
	}
	var doc map[string]any
	switch {
	case r.URL.Path == "/v1/apps" && r.URL.Query().Get("filter[bundleId]") == "":
		doc = map[string]any{"data": []any{}}
		if !f.noApp {
			doc["data"] = []any{map[string]any{"attributes": map[string]any{"bundleId": storeTestBundle}}}
		}
	case r.URL.Path == "/v1/apps":
		doc = map[string]any{"data": []any{}}
		if !f.noApp {
			doc["data"] = []any{map[string]any{"id": "6000000000", "attributes": map[string]any{
				"name": "Example", "bundleId": storeTestBundle,
			}}}
		}
	case r.URL.Path == "/v1/bundleIds":
		doc = map[string]any{"data": []any{}}
		if !f.noBundle {
			refs := []any{}
			for _, p := range f.profiles {
				refs = append(refs, map[string]any{"type": "profiles", "id": p["id"]})
			}
			doc = map[string]any{
				"data": []any{map[string]any{
					"id": "B1", "attributes": map[string]any{"identifier": storeTestBundle},
					"relationships": map[string]any{"profiles": map[string]any{"data": refs}},
				}},
				"included": f.profiles,
			}
		}
	case strings.HasSuffix(r.URL.Path, "/certificates"):
		doc = map[string]any{"data": []any{map[string]any{"id": "CERT123", "attributes": map[string]any{
			"name": f.certName, "certificateType": f.certType, "expirationDate": inDays(f.certDays),
		}}}}
	case r.URL.Path == "/v1/builds":
		doc = map[string]any{"data": []any{}}
		if !f.noBuild {
			doc["data"] = []any{map[string]any{"attributes": map[string]any{
				"version": "401", "processingState": f.buildState, "expired": f.buildDays <= 0,
				"uploadedDate": inDays(f.buildDays - 90), "expirationDate": inDays(f.buildDays),
			}}}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeAppStores) google(w http.ResponseWriter, r *http.Request) {
	f.note(r)
	if r.URL.Path == "/token" {
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(f.tokenBody))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"play-token-sentinel","token_type":"Bearer","expires_in":600}`))
		return
	}
	if f.playStatus != 0 {
		w.WriteHeader(f.playStatus)
		_, _ = w.Write([]byte(`{"error":{"status":"PERMISSION_DENIED","message":"The caller does not have permission"}}`))
		return
	}
	_, _ = w.Write([]byte(f.playBody))
}

// storeTestSecrets is a whole machine's worth, under repo scope.
func storeTestSecrets(t *testing.T) map[string]string {
	t.Helper()
	_, p8 := testKey(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	account, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "ci@example.iam.gserviceaccount.com",
		"private_key_id": "abc123",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		storeMatchPassword:             "match-sentinel-value",
		appstore.SecretKeyID:           "ABC123DEF4",
		appstore.SecretIssuerID:        testIssuer,
		appstore.SecretP8:              p8,
		"ANDROID_KEYSTORE_B64":         "a2V5c3RvcmUtc2VudGluZWw=",
		"ANDROID_KEYSTORE_PASSWORD":    "keystore-pass-sentinel",
		"ANDROID_KEY_ALIAS":            storeTestAlias,
		"ANDROID_KEY_PASSWORD":         "key-pass-sentinel",
		playstore.SecretServiceAccount: string(account),
	}
	scoped := make(map[string]string, len(values))
	for name, value := range values {
		scoped[secrets.ScopedName(name, "app")] = value
	}
	return scoped
}

// storeHarness is a checker with both stores stood in for and every probe
// answering for a healthy machine; a test breaks the one thing it is about.
type storeHarness struct {
	*storeChecker
	stores *fakeAppStores
	fake   *fakeSecrets
	cmds   map[string]fakeCmd
}

func newStoreHarness(t *testing.T) *storeHarness {
	t.Helper()
	dir := privateDir(t)
	cmds := healthyRunner("darwin")
	cmds["git ls-remote --heads --end-of-options "+storeTestStore] = fakeCmd{out: "abc\trefs/heads/" + storeTestBranch}
	cmds["git ls-remote --heads --end-of-options "+storeTestStore+" refs/heads/"+storeTestBranch] =
		fakeCmd{out: "abc\trefs/heads/" + storeTestBranch}

	d := newTestDoctor("darwin", dir, cmds, testConfig(dir), nil)
	d.clock = func() time.Time { return storeTestNow }
	fake := &fakeSecrets{values: storeTestSecrets(t), errs: map[string]error{}}
	d.newSecrets = fake.factory()

	stores := healthyStores()
	apple := httptest.NewServer(http.HandlerFunc(stores.apple))
	google := httptest.NewServer(http.HandlerFunc(stores.google))
	t.Cleanup(apple.Close)
	t.Cleanup(google.Close)
	d.newAppStore = func() *appstore.Client { return &appstore.Client{BaseURL: apple.URL, HTTP: apple.Client()} }

	return &storeHarness{
		storeChecker: &storeChecker{
			doctor: d,
			project: storeProject{
				dir: "packages/app", name: "Example", bundleID: storeTestBundle, pkg: storeTestBundle,
				match: matchStore{url: storeTestStore, branch: storeTestBranch},
			},
			where: dir, repoKey: "app", track: "internal",
			newPlay: func() *playstore.Client {
				return &playstore.Client{BaseURL: google.URL, TokenURL: google.URL + "/token", HTTP: google.Client()}
			},
			readUploadKey: func(context.Context, []byte, []byte, string) (playstore.UploadKey, error) {
				return playstore.UploadKey{Expires: storeTestNow.AddDate(20, 0, 0)}, nil
			},
		},
		stores: stores, fake: fake, cmds: cmds,
	}
}

func (h *storeHarness) mustCheck(t *testing.T, rep report, id string) check {
	t.Helper()
	c, ok := findCheck(rep, id)
	if !ok {
		var b bytes.Buffer
		printReport(&b, rep, false, false)
		t.Fatalf("no check %q in:\n%s", id, b.String())
	}
	return c
}

func wantLevel(t *testing.T, c check, want level) {
	t.Helper()
	if c.Level != want {
		t.Errorf("%s is %s, want %s: %s", c.ID, c.Level.marker(), want.marker(), c.Detail)
	}
}

func remedies(c check) string {
	return strings.Join(append(append([]string{}, c.Fix...), c.Manual...), "\n")
}

func TestStoreCheck_AHealthyProjectPassesEveryCheck(t *testing.T) {
	h := newStoreHarness(t)
	rep := h.run(t.Context())

	_, _, failed, _ := rep.counts()
	if failed != 0 {
		var b bytes.Buffer
		printReport(&b, rep, false, false)
		t.Fatalf("%d failed:\n%s", failed, b.String())
	}
	for _, id := range []string{
		"store.repo", "store.ios.secrets", "store.ios.match", "secret.asc.auth", "store.ios.app",
		"store.ios.profile", "store.ios.certificate", "store.ios.build",
		"store.android.secrets", "store.android.keystore", "store.android.account", "store.android.track",
	} {
		wantLevel(t, h.mustCheck(t, rep, id), levelPass)
	}
	if got := h.mustCheck(t, rep, "store.ios.app").Detail; !strings.Contains(got, "6000000000") {
		t.Errorf("the app record should give its id, got %q", got)
	}
	if got := h.mustCheck(t, rep, "store.android.track").Detail; !strings.Contains(got, "401") {
		t.Errorf("the track should give its version code, got %q", got)
	}
}

// The promise the whole command rests on. A check runs beside builds with keys
// that can change an account, so what it can send is the thing to pin: GETs,
// plus the one POST that trades an assertion for a token at Google's token
// endpoint. No edit, ever: a new edit kills the one a running upload holds.
func TestStoreCheck_OnlyEverReads(t *testing.T) {
	h := newStoreHarness(t)
	h.run(t.Context())

	seen := h.stores.seen()
	if len(seen) == 0 {
		t.Fatal("no request reached either store")
	}
	for _, r := range seen {
		method, path, _ := strings.Cut(r, " ")
		if method != http.MethodGet && r != "POST /token" {
			t.Errorf("%s: a store check may only read", r)
		}
		if strings.Contains(path, "edits") {
			t.Errorf("%s: a store check must never touch an edit", r)
		}
	}
}

func TestStoreCheck_AMissingSecretFailsAndNamesTheCommandForThisRepository(t *testing.T) {
	h := newStoreHarness(t)
	delete(h.fake.values, secrets.ScopedName("MATCH_PASSWORD", "app"))
	delete(h.fake.values, secrets.ScopedName("ANDROID_KEY_PASSWORD", "app"))
	rep := h.run(t.Context())

	ios := h.mustCheck(t, rep, "store.ios.secrets")
	wantLevel(t, ios, levelFail)
	if !strings.Contains(ios.Detail, "MATCH_PASSWORD") {
		t.Errorf("should name the secret: %q", ios.Detail)
	}
	if got := remedies(ios); !strings.Contains(got, "latem secrets set --repo app MATCH_PASSWORD") {
		t.Errorf("should give the command: %q", got)
	}
	android := h.mustCheck(t, rep, "store.android.secrets")
	wantLevel(t, android, levelFail)
	if got := remedies(android); !strings.Contains(got, "latem secrets set --repo app ANDROID_KEY_PASSWORD") {
		t.Errorf("should give the command: %q", got)
	}
}

func TestStoreCheck_TheSecretsTooLongForTheKeychainAreSentToTheFile(t *testing.T) {
	h := newStoreHarness(t)
	delete(h.fake.values, secrets.ScopedName("ANDROID_KEYSTORE_B64", "app"))
	rep := h.run(t.Context())

	got := remedies(h.mustCheck(t, rep, "store.android.secrets"))
	if !strings.Contains(got, "latem secrets set --store file --repo app ANDROID_KEYSTORE_B64") {
		t.Errorf("a keystore does not fit the Keychain, so its fix must say --store file: %q", got)
	}
}

func TestStoreCheck_SecretsAreLookedUpAsARunLooksThemUp(t *testing.T) {
	h := newStoreHarness(t)
	h.env = "staging"
	chain, err := h.resolver()
	if err != nil {
		t.Fatal(err)
	}
	h.fake.values = map[string]string{
		"NAME":                "machine",
		"app__NAME":           "repository",
		"app__staging__NAME":  "environment",
		"app__WIDER":          "repository",
		"app__staging__BLANK": "",
		"BLANK":               "machine",
	}
	for name, want := range map[string]string{"NAME": "environment", "WIDER": "repository", "BLANK": "machine"} {
		got, err := chain.Resolve(t.Context(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s came from %q, want %q", name, got, want)
		}
	}
	if _, err := chain.Resolve(t.Context(), "ABSENT"); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("an absent name should be ErrNotFound, got %v", err)
	}
}

func TestStoreCheck_ABrokenStoreIsNotReportedAsAMissingSecret(t *testing.T) {
	h := newStoreHarness(t)
	h.fake.errs[secrets.ScopedName("MATCH_PASSWORD", "app")] = errors.New("the keychain is locked")
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.ios.secrets")
	wantLevel(t, c, levelFail)
	if !strings.Contains(c.Detail, "could not read MATCH_PASSWORD") {
		t.Errorf("should say it could not read it: %q", c.Detail)
	}
	if strings.Contains(remedies(c), "latem secrets set") {
		t.Errorf("setting a value will not unlock a Keychain: %q", remedies(c))
	}
}

func TestStoreCheck_AKeyAppleRefusesFailsAndStopsAskingApple(t *testing.T) {
	h := newStoreHarness(t)
	h.newAppStore = func() *appstore.Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.stores.note(r)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		return &appstore.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	}
	rep := h.run(t.Context())

	auth := h.mustCheck(t, rep, "secret.asc.auth")
	wantLevel(t, auth, levelFail)
	if len(auth.Fix) == 0 {
		t.Error("a wrong key must name a fix")
	}
	wantLevel(t, h.mustCheck(t, rep, "store.ios.asc"), levelSkip)
	if _, ok := findCheck(rep, "store.ios.app"); ok {
		t.Error("should not have gone on to ask about the app")
	}
}

func TestStoreCheck_AKeyGoogleRefusesFailsAndNamesTheKey(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.tokenStatus = http.StatusBadRequest
	h.stores.tokenBody = `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.android.account")
	wantLevel(t, c, levelFail)
	if !strings.Contains(c.Detail, "Invalid JWT Signature") {
		t.Errorf("should give Google's words: %q", c.Detail)
	}
	if got := remedies(c); !strings.Contains(got, "abc123") || !strings.Contains(got, "latem secrets set") {
		t.Errorf("should name the key and how to replace it: %q", got)
	}
	if _, ok := findCheck(rep, "store.android.track"); ok {
		t.Error("should not have gone on to read the track")
	}
}

func TestStoreCheck_ADamagedServiceAccountKeyNeverQuotesItself(t *testing.T) {
	h := newStoreHarness(t)
	h.fake.values[secrets.ScopedName(playstore.SecretServiceAccount, "app")] = "x-key-material-sentinel"
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.android.account")
	wantLevel(t, c, levelFail)
	if strings.Contains(c.Detail, "key-material-sentinel") {
		t.Fatalf("the report quoted the key: %q", c.Detail)
	}
}

func TestStoreCheck_ADateWarnsInsideThirtyDaysAndFailsInsideSeven(t *testing.T) {
	cases := []struct {
		days int
		want level
	}{
		{300, levelPass}, {31, levelPass}, {30, levelWarn}, {8, levelWarn}, {7, levelFail}, {1, levelFail}, {-1, levelFail},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d days", tc.days), func(t *testing.T) {
			h := newStoreHarness(t)
			h.stores.certDays = tc.days
			h.stores.buildDays = tc.days
			h.stores.profiles[0]["attributes"].(map[string]any)["expirationDate"] = inDays(tc.days)
			h.readUploadKey = func(context.Context, []byte, []byte, string) (playstore.UploadKey, error) {
				return playstore.UploadKey{Expires: storeTestNow.AddDate(0, 0, tc.days)}, nil
			}
			rep := h.run(t.Context())

			for _, id := range []string{
				"store.ios.certificate", "store.ios.profile", "store.ios.build", "store.android.keystore",
			} {
				c := h.mustCheck(t, rep, id)
				wantLevel(t, c, tc.want)
				if tc.want != levelPass && remedies(c) == "" {
					t.Errorf("%s: a date this close must name a fix", id)
				}
				if tc.want == levelPass && remedies(c) != "" {
					t.Errorf("%s: a healthy date should offer no fix: %q", id, remedies(c))
				}
			}
		})
	}
}

func TestStoreCheck_SaysHowManyDaysAreLeft(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.certDays = 7
	if got := h.mustCheck(t, h.run(t.Context()), "store.ios.certificate").Detail; !strings.Contains(got, "in 7 days") {
		t.Errorf("got %q", got)
	}
	h = newStoreHarness(t)
	h.stores.certDays = 1
	if got := h.mustCheck(t, h.run(t.Context()), "store.ios.certificate").Detail; !strings.Contains(got, "in 1 day,") && !strings.HasSuffix(got, "in 1 day") {
		t.Errorf("got %q", got)
	}
}

func TestStoreCheck_ADateThatCannotBeReadWarnsAndIsNotAMissingThing(t *testing.T) {
	h := newStoreHarness(t)
	h.readUploadKey = func(context.Context, []byte, []byte, string) (playstore.UploadKey, error) {
		return playstore.UploadKey{}, nil
	}
	wantLevel(t, h.mustCheck(t, h.run(t.Context()), "store.android.keystore"), levelWarn)

	got := func() check {
		h := newStoreHarness(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/certificates") {
				_, _ = w.Write([]byte(`{"data":[{"id":"C1","attributes":{"name":"Apple Distribution",
					"certificateType":"DISTRIBUTION","expirationDate":"some day"}}]}`))
				return
			}
			h.stores.apple(w, r)
		}))
		t.Cleanup(srv.Close)
		h.newAppStore = func() *appstore.Client { return &appstore.Client{BaseURL: srv.URL, HTTP: srv.Client()} }
		return h.mustCheck(t, h.run(t.Context()), "store.ios.certificate")
	}()
	wantLevel(t, got, levelWarn)
}

func TestStoreCheck_AStoreHavingABadMinuteDoesNotFailTheProject(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.appleStatus = http.StatusServiceUnavailable
	h.stores.playStatus = http.StatusTooManyRequests
	rep := h.run(t.Context())

	wantLevel(t, h.mustCheck(t, rep, "store.ios.app"), levelWarn)
	wantLevel(t, h.mustCheck(t, rep, "store.android.track"), levelWarn)
}

func TestStoreCheck_AReleaseWithNoArtifactIsStillDescribed(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.playBody = `{"releases":[{"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_DRAFT"}]}`
	c := h.mustCheck(t, h.run(t.Context()), "store.android.track")
	wantLevel(t, c, levelWarn)
	if strings.Contains(c.Detail, "-1") {
		t.Errorf("no version code to give: %q", c.Detail)
	}
}

func TestStoreCheck_EitherNameOfTheDistributionCertificatePasses(t *testing.T) {
	for kind, name := range map[string]string{
		"DISTRIBUTION":     "Apple Distribution: Example",
		"IOS_DISTRIBUTION": "iPhone Distribution: Example",
	} {
		t.Run(kind, func(t *testing.T) {
			h := newStoreHarness(t)
			h.stores.certType, h.stores.certName = kind, name
			rep := h.run(t.Context())

			c := h.mustCheck(t, rep, "store.ios.certificate")
			wantLevel(t, c, levelPass)
			if !strings.Contains(c.Detail, kind) || !strings.Contains(c.Detail, name) {
				t.Errorf("should say both the name and the type: %q", c.Detail)
			}
		})
	}
}

func TestStoreCheck_ACertificateOfAnotherKindIsNoAnswer(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.certType = "DEVELOPMENT"
	rep := h.run(t.Context())
	wantLevel(t, h.mustCheck(t, rep, "store.ios.certificate"), levelFail)
}

func TestStoreCheck_ReadsMatchsProfileEvenWhenAnotherLastsLonger(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.profiles = []map[string]any{
		storeProfile("eas", "*[expo] "+storeTestBundle, "IOS_APP_STORE", "ACTIVE", 900),
		storeProfile("dev", "Development", "IOS_APP_DEVELOPMENT", "ACTIVE", 2000),
		storeProfile("m", "match AppStore "+storeTestBundle, "IOS_APP_STORE", "ACTIVE", 300),
	}
	rep := h.run(t.Context())

	if got := h.mustCheck(t, rep, "store.ios.profile").Detail; !strings.HasPrefix(got, "match AppStore") {
		t.Errorf("should be match's profile, got %q", got)
	}
}

func TestStoreCheck_WithoutMatchTheActiveProfileThatLastsLongestIsTheAnswer(t *testing.T) {
	got, ok := signingProfile([]appstore.Profile{
		{ID: "dev", Type: "IOS_APP_DEVELOPMENT", State: "ACTIVE", Expires: storeTestNow.AddDate(9, 0, 0)},
		{ID: "dead", Type: "IOS_APP_STORE", State: "INVALID", Expires: storeTestNow.AddDate(9, 0, 0)},
		{ID: "soon", Type: "IOS_APP_STORE", State: "ACTIVE", Expires: storeTestNow.AddDate(0, 1, 0)},
		{ID: "late", Type: "IOS_APP_STORE", State: "ACTIVE", Expires: storeTestNow.AddDate(1, 0, 0)},
	}, storeTestBundle)
	if !ok || got.ID != "late" {
		t.Errorf("picked %q", got.ID)
	}
}

func TestStoreCheck_AnInvalidProfileFailsWhateverItsDate(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.profiles = []map[string]any{storeProfile("p", "match AppStore "+storeTestBundle, "IOS_APP_STORE", "INVALID", 300)}
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.ios.profile")
	wantLevel(t, c, levelFail)
	if !strings.Contains(remedies(c), "not on a runner") {
		t.Errorf("runners never create signing: the fix must say so: %q", remedies(c))
	}
}

func TestStoreCheck_NoProfileAndNoBundleIDEachSaySo(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.profiles = nil
	wantLevel(t, h.mustCheck(t, h.run(t.Context()), "store.ios.profile"), levelFail)

	h = newStoreHarness(t)
	h.stores.noBundle = true
	c := h.mustCheck(t, h.run(t.Context()), "store.ios.profile")
	wantLevel(t, c, levelFail)
	if !strings.Contains(c.Detail, "not registered") {
		t.Errorf("should say the bundle ID is not registered: %q", c.Detail)
	}
}

func TestStoreCheck_AnAppRecordIsAPersonsStep(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.noApp = true
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.ios.app")
	wantLevel(t, c, levelFail)
	if len(c.Fix) != 0 || !strings.Contains(strings.Join(c.Manual, " "), storeTestBundle) {
		t.Errorf("no command makes an app record, so this must be by hand, with the bundle ID: %+v", c)
	}
	if _, ok := findCheck(rep, "store.ios.build"); ok {
		t.Error("with no app there is no build to ask about")
	}
	if _, ok := findCheck(rep, "store.ios.profile"); !ok {
		t.Error("signing is a separate question and should still be answered")
	}
}

func TestStoreCheck_AnUnsignedAgreementNamesTheAccountHolder(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.appleStatus = http.StatusForbidden
	h.stores.appleBody = `{"errors":[{"status":"403","code":"FORBIDDEN_ERROR",
		"title":"A required agreement is missing or has expired.",
		"detail":"This request requires an in-effect agreement that has not been signed or has expired."}]}`
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.ios.app")
	wantLevel(t, c, levelFail)
	if !strings.Contains(remedies(c), "Account Holder") {
		t.Errorf("only the Account Holder can clear this: %q", remedies(c))
	}
}

func TestStoreCheck_ALastUploadTheStoreRejectedFails(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.buildState = "INVALID"
	rep := h.run(t.Context())
	wantLevel(t, h.mustCheck(t, rep, "store.ios.build"), levelFail)
}

func TestStoreCheck_AnAppWithNoUploadYetOnlyWarns(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.noBuild = true
	h.stores.playBody = `{}`
	rep := h.run(t.Context())

	wantLevel(t, h.mustCheck(t, rep, "store.ios.build"), levelWarn)
	wantLevel(t, h.mustCheck(t, rep, "store.android.track"), levelWarn)
}

func TestStoreCheck_ASigningStoreBranchThatIsNotThereFails(t *testing.T) {
	h := newStoreHarness(t)
	// What ssh writes to stderr, which is in the output a probe gets back.
	h.cmds["git ls-remote --heads --end-of-options "+storeTestStore+" refs/heads/"+storeTestBranch] =
		fakeCmd{out: "Warning: Permanently added 'github.com' to the list of known hosts.\n"}
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.ios.match")
	wantLevel(t, c, levelFail)
	if !strings.Contains(c.Detail, storeTestBranch) {
		t.Errorf("should name the branch: %q", c.Detail)
	}
}

func TestStoreCheck_ASigningStoreThisMachineCannotReadFails(t *testing.T) {
	h := newStoreHarness(t)
	delete(h.cmds, "git ls-remote --heads --end-of-options "+storeTestStore)
	wantLevel(t, h.mustCheck(t, h.run(t.Context()), "store.ios.match"), levelFail)
}

// Found on the machine that uploads Tsedal: run over ssh, this failed, and named
// a fix that could only have made things worse.
func TestStoreCheck_ASigningStoreThisSessionCannotAskIsNotAFailure(t *testing.T) {
	h := newStoreHarness(t)
	const noLogin = "fatal: could not read Username for 'https://github.com': terminal prompts disabled"
	h.cmds["launchctl managername"] = fakeCmd{out: "Background"}
	h.cmds["env -u SSH_AUTH_SOCK git ls-remote --heads --end-of-options "+storeTestStore] = fakeCmd{exit: 128, out: noLogin}
	h.cmds["git ls-remote --heads --end-of-options "+storeTestStore] = fakeCmd{exit: 128, out: noLogin}
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.ios.match")
	wantLevel(t, c, levelWarn)
	if len(c.Fix) != 0 {
		t.Errorf("no fix is known to be needed: %q", c.Fix)
	}
}

func TestStoreCheck_AMatchfileURLThatIsNotGitOverAWireIsNeverPassedToGit(t *testing.T) {
	for _, url := range []string{"--upload-pack=/bin/sh", "ext::sh -c id", "file:///tmp/x", "/tmp/store"} {
		h := newStoreHarness(t)
		h.project.match.url = url
		// Nothing registered, so a call would fail the check; a warn says none was made.
		rep := h.run(t.Context())
		wantLevel(t, h.mustCheck(t, rep, "store.ios.match"), levelWarn)
	}
}

func TestStoreCheck_ProjectWithoutMatchDoesNotAskForItsPassword(t *testing.T) {
	h := newStoreHarness(t)
	h.project.match = matchStore{}
	delete(h.fake.values, secrets.ScopedName("MATCH_PASSWORD", "app"))
	rep := h.run(t.Context())

	wantLevel(t, h.mustCheck(t, rep, "store.ios.secrets"), levelPass)
	wantLevel(t, h.mustCheck(t, rep, "store.ios.match"), levelSkip)
}

func TestStoreCheck_TheUploadKeysFailuresEachNameTheirSecret(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"password": {playstore.ErrKeystorePassword, "ANDROID_KEYSTORE_PASSWORD"},
		"alias":    {playstore.ErrNoAlias, "ANDROID_KEY_ALIAS"},
		"keystore": {errors.New("it is not base64"), "ANDROID_KEYSTORE_B64"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newStoreHarness(t)
			h.readUploadKey = func(context.Context, []byte, []byte, string) (playstore.UploadKey, error) {
				return playstore.UploadKey{}, tc.err
			}
			c := h.mustCheck(t, h.run(t.Context()), "store.android.keystore")
			wantLevel(t, c, levelFail)
			if !strings.Contains(remedies(c), tc.want) {
				t.Errorf("should name %s: %q", tc.want, remedies(c))
			}
		})
	}
}

func TestStoreCheck_AMachineWithNoJDKSkipsTheUploadKey(t *testing.T) {
	h := newStoreHarness(t)
	h.readUploadKey = func(context.Context, []byte, []byte, string) (playstore.UploadKey, error) {
		return playstore.UploadKey{}, playstore.ErrNoKeytool
	}
	wantLevel(t, h.mustCheck(t, h.run(t.Context()), "store.android.keystore"), levelSkip)
}

func TestStoreCheck_AServiceAccountThatCannotSeeTheAppNamesTheInvite(t *testing.T) {
	h := newStoreHarness(t)
	h.stores.playStatus = http.StatusForbidden
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.android.track")
	wantLevel(t, c, levelFail)
	got := remedies(c)
	if !strings.Contains(got, "ci@example.iam.gserviceaccount.com") || !strings.Contains(got, storeTestBundle) {
		t.Errorf("should say who to invite to what: %q", got)
	}
	wantLevel(t, h.mustCheck(t, rep, "store.android.account"), levelPass)
}

func TestStoreCheck_TheNewestReleaseIsTheOneThatCounts(t *testing.T) {
	cases := map[string]struct {
		body string
		want level
	}{
		"published": {`{"releases":[
			{"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_NOT_APPROVED","activeArtifacts":[{"versionCode":3}]},
			{"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_PUBLISHED","activeArtifacts":[{"versionCode":9}]}]}`, levelPass},
		"rejected": {`{"releases":[
			{"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_PUBLISHED","activeArtifacts":[{"versionCode":3}]},
			{"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_NOT_APPROVED","activeArtifacts":[{"versionCode":9}]}]}`, levelFail},
		"draft": {`{"releases":[
			{"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_DRAFT","activeArtifacts":[{"versionCode":9}]}]}`, levelWarn},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newStoreHarness(t)
			h.stores.playBody = tc.body
			c := h.mustCheck(t, h.run(t.Context()), "store.android.track")
			wantLevel(t, c, tc.want)
			if !strings.Contains(c.Detail, "version code 9") {
				t.Errorf("should be about version code 9: %q", c.Detail)
			}
		})
	}
}

func TestStoreCheck_ReadsTheTrackItIsToldTo(t *testing.T) {
	h := newStoreHarness(t)
	h.track = "alpha"
	h.run(t.Context())

	want := "GET /androidpublisher/v3/applications/" + storeTestBundle + "/tracks/alpha/releases"
	if got := strings.Join(h.stores.seen(), "\n"); !strings.Contains(got, want) {
		t.Errorf("no %q in:\n%s", want, got)
	}
}

func TestStoreCheck_PlatformLimitsTheReport(t *testing.T) {
	h := newStoreHarness(t)
	h.platform = "android"
	rep := h.run(t.Context())

	if _, ok := findCheck(rep, "store.ios.secrets"); ok {
		t.Error("--platform android should not check iOS")
	}
	h.mustCheck(t, rep, "store.android.secrets")
}

func TestStoreCheck_AProjectWithOnePlatformChecksThatOne(t *testing.T) {
	h := newStoreHarness(t)
	h.project.pkg = ""
	rep := h.run(t.Context())

	wantLevel(t, h.mustCheck(t, rep, "store.identifier.android"), levelSkip)
	if _, ok := findCheck(rep, "store.android.secrets"); ok {
		t.Error("a project with no package has no Android to check")
	}
}

func TestStoreCheck_AskingForAPlatformTheProjectDoesNotNameFails(t *testing.T) {
	h := newStoreHarness(t)
	h.project.bundleID = ""
	h.platform = "ios"
	wantLevel(t, h.mustCheck(t, h.run(t.Context()), "store.identifier.ios"), levelFail)
}

func TestStoreCheck_AppConfigIsNamedWhenItHidesTheIdentifiers(t *testing.T) {
	h := newStoreHarness(t)
	h.project = storeProject{dir: ".", dynamic: "app.config.ts"}
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.identifiers")
	wantLevel(t, c, levelFail)
	if !strings.Contains(strings.Join(c.Notes, " "), "app.config.ts") {
		t.Errorf("should say why it could not read them: %+v", c)
	}
	if !strings.Contains(remedies(c), "--bundle-id") {
		t.Errorf("should give the flags: %q", remedies(c))
	}
}

func TestStoreCheck_AnIdentifierThatIsNotOneIsNotSentToAStore(t *testing.T) {
	h := newStoreHarness(t)
	h.project.bundleID = "../../v1/users"
	rep := h.run(t.Context())

	wantLevel(t, h.mustCheck(t, rep, "store.identifier.ios"), levelFail)
	if got := h.stores.seen(); len(got) != 0 {
		t.Errorf("reached a store: %v", got)
	}
}

func TestStoreCheck_AMachineNotBuildingTheRepositorySaysWhySecretsAreMissing(t *testing.T) {
	h := newStoreHarness(t)
	h.repoKey, h.slug = "", "example/app"
	h.fake.values = map[string]string{}
	rep := h.run(t.Context())

	c := h.mustCheck(t, rep, "store.repo")
	wantLevel(t, c, levelWarn)
	if !strings.Contains(remedies(c), "latem repo add example/app") {
		t.Errorf("should give the command: %q", remedies(c))
	}
	if got := remedies(h.mustCheck(t, rep, "store.ios.secrets")); strings.Contains(got, "--repo") {
		t.Errorf("no repository to scope to: %q", got)
	}
}

// INV-4 applies to a diagnostic as much as to a log. Every value goes through
// this command, so every value is looked for in everything it prints, in both
// formats and in the states that print the most.
func TestStoreCheck_NeverPrintsAValue(t *testing.T) {
	for name, breakIt := range map[string]func(*storeHarness){
		"healthy": func(*storeHarness) {},
		"refused everywhere": func(h *storeHarness) {
			h.stores.appleStatus = http.StatusForbidden
			h.stores.appleBody = `{"errors":[{"detail":"no"}]}`
			h.stores.playStatus = http.StatusForbidden
		},
		"token refused": func(h *storeHarness) {
			h.stores.tokenStatus = http.StatusBadRequest
			h.stores.tokenBody = `{"error":"invalid_grant"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newStoreHarness(t)
			breakIt(h)
			rep := h.run(t.Context())

			var text, doc bytes.Buffer
			printReport(&text, rep, true, false)
			if err := printStoreJSON(&doc, h.storeChecker, rep); err != nil {
				t.Fatal(err)
			}
			output := text.String() + doc.String()
			for key, value := range storeSentinels(h) {
				if strings.Contains(output, value) {
					t.Errorf("the report printed %s:\n%s", key, output)
				}
			}
			if strings.Contains(output, "play-token-sentinel") {
				t.Errorf("the report printed an access token:\n%s", output)
			}
		})
	}
}

// storeSentinels is the values that must not appear, leaving out the two
// identifiers a report prints on purpose.
func storeSentinels(h *storeHarness) map[string]string {
	out := map[string]string{}
	for key, value := range h.fake.values {
		if strings.HasSuffix(key, appstore.SecretKeyID) || strings.HasSuffix(key, appstore.SecretIssuerID) {
			continue
		}
		out[key] = value
		if strings.HasSuffix(key, appstore.SecretP8) {
			out[key+" body"] = strings.Split(value, "\n")[1]
		}
	}
	return out
}

func TestStoreCheck_JSONIsTheSameReport(t *testing.T) {
	h := newStoreHarness(t)
	delete(h.fake.values, secrets.ScopedName("MATCH_PASSWORD", "app"))
	rep := h.run(t.Context())

	var buf bytes.Buffer
	if err := printStoreJSON(&buf, h.storeChecker, rep); err != nil {
		t.Fatal(err)
	}
	var got storeJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	_, _, failed, _ := rep.counts()
	if got.Failed != failed || failed == 0 {
		t.Errorf("failed = %d, the report has %d", got.Failed, failed)
	}
	if got.App.BundleID != storeTestBundle || got.RepoKey != "app" {
		t.Errorf("identifiers: %+v", got)
	}
	var levels []string
	for _, sec := range got.Sections {
		for _, c := range sec.Checks {
			if c.ID == "store.ios.secrets" {
				levels = append(levels, c.Level)
			}
		}
	}
	if len(levels) != 1 || levels[0] != "fail" {
		t.Errorf("store.ios.secrets: %v", levels)
	}
}

// ------------------------------------------------------------ the command

func TestStoreCheckCommand_RefusesWhatItCannotMean(t *testing.T) {
	dir := configDirWith(t, "")
	for _, args := range [][]string{
		{"store", "check", "--offline"},
		{"store", "check", "--offline", "--repo", "app", "."},
		{"store", "check", "--platform", "windows"},
		{"store", "check", "a", "b"},
		{"store", "nosuch"},
	} {
		_, err := latemIn(t, dir, "", args...)
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != ExitUsage {
			t.Errorf("%v: want a usage error, got %v", args, err)
		}
	}
}

func TestStoreCheckCommand_AConfigThatDoesNotParseIsNotAMachineWithNoRepositories(t *testing.T) {
	dir := configDirWith(t, "this is not toml")
	fakeOrigin(t, "")
	_, err := latemIn(t, dir, "", "store", "check", t.TempDir())
	if err == nil || strings.Contains(err.Error(), "no Expo app") {
		t.Errorf("should have stopped at the config: %v", err)
	}
}

func TestStoreCheckCommand_SaysWhenThereIsNoExpoApp(t *testing.T) {
	dir := configDirWith(t, "")
	fakeOrigin(t, "")
	_, err := latemIn(t, dir, "", "store", "check", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no Expo app") {
		t.Errorf("got %v", err)
	}
}

func TestStoreCheckCommand_HelpPromisesItOnlyReads(t *testing.T) {
	out, err := latemIn(t, configDirWith(t, ""), "", "store", "check", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Read-only", "never opens a Play edit", "INV-4"} {
		if !strings.Contains(out, want) {
			t.Errorf("help is missing %q", want)
		}
	}
}

// ---------------------------------------------------------- reading a project

func TestStoreProject_FindsTheAppBelowAMonorepoRoot(t *testing.T) {
	fsys := fstest.MapFS{
		"package.json":                  {Data: []byte(`{}`)},
		"packages/app/app.json":         {Data: []byte(`{"expo":{"name":"Example","ios":{"bundleIdentifier":"com.example.app"},"android":{"package":"com.example.android"}}}`)},
		"packages/app/fastlane/Appfile": {Data: []byte("team_id(\"AAAA111111\")\n")},
		"packages/app/fastlane/Matchfile": {Data: []byte(`
# git_url("https://example.com/not-this.git")
git_url(ENV["MATCH_GIT_URL"] || "https://github.com/example/signing.git")
storage_mode("git")
team_id("BBBB222222")
git_branch('team-BBBB222222')
`)},
		"node_modules/expo-template/app.json": {Data: []byte(`{"expo":{"name":"dep"}}`)},
		"packages/app/ios/app.json":           {Data: []byte(`{"expo":{"name":"generated"}}`)},
		"packages/web/app.json":               {Data: []byte(`{"name":"not expo"}`)},
	}
	p, err := readStoreProject(fsys, "")
	if err != nil {
		t.Fatal(err)
	}
	want := storeProject{
		dir: "packages/app", name: "Example", bundleID: "com.example.app", pkg: "com.example.android",
		team:  "BBBB222222",
		match: matchStore{url: "https://github.com/example/signing.git", branch: "team-BBBB222222"},
	}
	if p != want {
		t.Errorf("got  %+v\nwant %+v", p, want)
	}
}

func TestStoreProject_SeveralAppsAreAskedAbout(t *testing.T) {
	fsys := fstest.MapFS{
		"a/app.json": {Data: []byte(`{"expo":{"name":"A"}}`)},
		"b/app.json": {Data: []byte(`{"expo":{"name":"B"}}`)},
	}
	_, err := readStoreProject(fsys, "")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != ExitUsage || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("got %v", err)
	}
	p, err := readStoreProject(fsys, "b")
	if err != nil || p.name != "B" {
		t.Errorf("--app b: %+v, %v", p, err)
	}
}

func TestStoreProject_FastlaneAtTheRootIsFound(t *testing.T) {
	fsys := fstest.MapFS{
		"apps/mobile/app.json": {Data: []byte(`{"expo":{"name":"A"}}`)},
		"fastlane/Matchfile":   {Data: []byte(`git_url("git@github.com:example/signing.git")`)},
	}
	p, err := readStoreProject(fsys, "")
	if err != nil || p.match.url != "git@github.com:example/signing.git" {
		t.Errorf("%+v, %v", p, err)
	}
}

func TestStoreProject_AnAppConfigIsNotedAndNotRun(t *testing.T) {
	p, err := readStoreProject(fstest.MapFS{"app.config.ts": {Data: []byte(`export default {}`)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.dynamic != "app.config.ts" || p.bundleID != "" {
		t.Errorf("%+v", p)
	}
}

func TestStoreProject_ABrokenAppJSONIsAnError(t *testing.T) {
	_, err := readStoreProject(fstest.MapFS{"app.json": {Data: []byte(`{`)}}, ".")
	if err == nil || !strings.Contains(err.Error(), "app.json") {
		t.Errorf("got %v", err)
	}
}

func TestStoreProject_FlagsOverrideTheFiles(t *testing.T) {
	fsys := fstest.MapFS{"app.json": {Data: []byte(`{"expo":{"ios":{"bundleIdentifier":"com.example.a"}}}`)}}
	p, err := storeProjectFor(fsys, storeCheckFlags{bundleID: "com.example.b"})
	if err != nil || p.bundleID != "com.example.b" {
		t.Errorf("%+v, %v", p, err)
	}
}

func TestStoreProject_BothIdentifiersNeedNoExpoProject(t *testing.T) {
	p, err := storeProjectFor(fstest.MapFS{}, storeCheckFlags{bundleID: "com.example.b", pkg: "com.example.b"})
	if err != nil || p.pkg != "com.example.b" {
		t.Errorf("%+v, %v", p, err)
	}
}

func TestFastlaneValue(t *testing.T) {
	for _, tc := range []struct{ text, key, want string }{
		{`team_id("X")`, "team_id", "X"},
		{`team_id "X"`, "team_id", "X"},
		{`  team_id('X')  `, "team_id", "X"},
		{`app_identifier(["a.b"])`, "app_identifier", "a.b"},
		{`git_url(ENV["URL"] || "https://x")`, "git_url", "https://x"},
		{`# team_id("X")`, "team_id", ""},
		{`itc_team_id("X")`, "team_id", ""},
		{`team_id_other("X")`, "team_id", ""},
		{`team_id(ENV.fetch(key))`, "team_id", ""},
	} {
		if got := fastlaneValue(tc.text, tc.key); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
}
