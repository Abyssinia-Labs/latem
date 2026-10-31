package appstore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func TestAppByBundleID_IsNotSatisfiedByALongerIdentifier(t *testing.T) {
	c := readClient(t, answer(`{"data":[
		{"id":"1","attributes":{"name":"Widget","bundleId":"com.example.app.widget"}},
		{"id":"2","attributes":{"name":"App","bundleId":"com.example.app"}}]}`))
	app, found, err := c.AppByBundleID(t.Context(), "t", "com.example.app")
	if err != nil || !found || app.ID != "2" || app.Name != "App" {
		t.Errorf("%+v, %v, %v", app, found, err)
	}

	_, found, err = c.AppByBundleID(t.Context(), "t", "com.example")
	if err != nil || found {
		t.Errorf("a prefix must not match: %v, %v", found, err)
	}
}

func TestAppByBundleID_AsksByFilterAndForNoContent(t *testing.T) {
	var query string
	c := readClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("filter[bundleId]")
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	if _, _, err := c.AppByBundleID(t.Context(), "tok", "com.example.app"); err != nil {
		t.Fatal(err)
	}
	if query != "com.example.app" {
		t.Errorf("filter = %q", query)
	}
}

func TestBundleIDByIdentifier_KeepsOnlyItsOwnProfiles(t *testing.T) {
	var fields string
	c := readClient(t, func(w http.ResponseWriter, r *http.Request) {
		fields = r.URL.Query().Get("fields[profiles]")
		_, _ = w.Write([]byte(`{
		"data":[{"id":"B1","attributes":{"identifier":"com.example.app"},
			"relationships":{"profiles":{"data":[{"type":"profiles","id":"P1"}]}}}],
		"included":[
			{"type":"profiles","id":"P1","attributes":{"name":"match AppStore com.example.app",
				"profileType":"IOS_APP_STORE","profileState":"ACTIVE","expirationDate":"2027-10-30T12:00:00.000+00:00"}},
			{"type":"profiles","id":"P2","attributes":{"name":"another bundle's"}},
			{"type":"bundleIdCapabilities","id":"P1","attributes":{}}]}`))
	})
	b, found, err := c.BundleIDByIdentifier(t.Context(), "t", "com.example.app")
	if err != nil || !found {
		t.Fatalf("%v, %v", found, err)
	}
	if len(b.Profiles) != 1 || b.Profiles[0].ID != "P1" {
		t.Fatalf("profiles: %+v", b.Profiles)
	}
	want := time.Date(2027, 10, 30, 12, 0, 0, 0, time.UTC)
	if p := b.Profiles[0]; p.State != "ACTIVE" || p.Type != "IOS_APP_STORE" || !p.Expires.Equal(want) {
		t.Errorf("%+v", p)
	}
	if strings.Contains(fields, "profileContent") {
		t.Errorf("a check has no use for a profile's contents: %q", fields)
	}
}

func TestProfileCertificates_ReadsTheRealName(t *testing.T) {
	var path, fields string
	c := readClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, fields = r.URL.Path, r.URL.Query().Get("fields[certificates]")
		_, _ = w.Write([]byte(`{"data":[{"id":"C1","attributes":{"name":"iPhone Distribution: Example",
			"certificateType":"IOS_DISTRIBUTION","expirationDate":"2027-01-02T03:04:05.000+00:00"}}]}`))
	})
	certs, err := c.ProfileCertificates(t.Context(), "t", "P1")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/profiles/P1/certificates" {
		t.Errorf("path = %q", path)
	}
	if len(certs) != 1 || certs[0].Type != "IOS_DISTRIBUTION" || certs[0].Expires.IsZero() {
		t.Errorf("%+v", certs)
	}
	if strings.Contains(fields, "certificateContent") {
		t.Errorf("a check has no use for a certificate's contents: %q", fields)
	}
}

func TestLatestBuild_AsksForTheNewestUpload(t *testing.T) {
	var sort, limit, app string
	c := readClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sort, limit, app = q.Get("sort"), q.Get("limit"), q.Get("filter[app]")
		_, _ = w.Write([]byte(`{"data":[{"attributes":{"version":"501","processingState":"VALID",
			"uploadedDate":"2026-10-30T15:20:30-07:00","expirationDate":"2027-01-28T15:20:30-08:00","expired":false}}]}`))
	})
	b, found, err := c.LatestBuild(t.Context(), "t", "42")
	if err != nil || !found {
		t.Fatalf("%v, %v", found, err)
	}
	if sort != "-uploadedDate" || limit != "1" || app != "42" {
		t.Errorf("sort=%q limit=%q app=%q", sort, limit, app)
	}
	if b.Version != "501" || b.State != "VALID" || b.Expired || b.Uploaded.IsZero() || b.Expires.IsZero() {
		t.Errorf("%+v", b)
	}
}

func TestLatestBuild_NoBuildsIsNotAnError(t *testing.T) {
	c := readClient(t, answer(`{"data":[]}`))
	if _, found, err := c.LatestBuild(t.Context(), "t", "42"); err != nil || found {
		t.Errorf("%v, %v", found, err)
	}
}

func TestReads_ARefusalKeepsApplesWords(t *testing.T) {
	c := readClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"status":"403","code":"FORBIDDEN_ERROR",
			"title":"This request is forbidden for security reasons",
			"detail":"The API key in use does not allow this request"}]}`))
	})
	_, _, err := c.AppByBundleID(t.Context(), "t", "com.example.app")
	e, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("got %v", err)
	}
	if e.Status != http.StatusForbidden || e.Code != "FORBIDDEN_ERROR" {
		t.Errorf("%+v", e)
	}
	if !strings.Contains(e.Error(), "does not allow this request") {
		t.Errorf("Error() = %q", e.Error())
	}
	if e.AgreementMissing() {
		t.Error("this refusal has nothing to do with an agreement")
	}
}

func TestReads_ARefusalWithNoBodyStillSaysItsStatus(t *testing.T) {
	c := readClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	_, _, err := c.LatestBuild(t.Context(), "t", "42")
	e, ok := AsAPIError(err)
	if !ok || e.Status != http.StatusBadGateway || e.Error() != "HTTP 502" {
		t.Errorf("%v", err)
	}
}

func TestAPIError_AgreementMissingReadsApplesWords(t *testing.T) {
	for _, e := range []APIError{
		{Title: "A required agreement is missing or has expired."},
		{Code: "FORBIDDEN.REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED"},
		{Detail: "This request requires an in-effect agreement that has not been signed or has expired."},
	} {
		if !e.AgreementMissing() {
			t.Errorf("%+v", e)
		}
	}
}

func TestReads_EveryReadIsAGet(t *testing.T) {
	var methods []string
	c := readClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	ctx := t.Context()
	_, _, _ = c.AppByBundleID(ctx, "t", "com.example.app")
	_, _, _ = c.BundleIDByIdentifier(ctx, "t", "com.example.app")
	_, _ = c.ProfileCertificates(ctx, "t", "P1")
	_, _, _ = c.LatestBuild(ctx, "t", "42")
	if len(methods) != 4 {
		t.Fatalf("%d requests", len(methods))
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("method %s", m)
		}
	}
}

func TestParseTime_ReadsApplesFormsAndGivesUpQuietly(t *testing.T) {
	for _, s := range []string{
		"2027-10-30T12:00:00.000+00:00", "2027-10-30T05:00:00-07:00", "2027-10-30T12:00:00.000+0000", "2027-10-30T12:00:00Z",
	} {
		if !parseTime(s).Equal(time.Date(2027, 10, 30, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("%q: %v", s, parseTime(s))
		}
	}
	if !parseTime("next year").IsZero() || !parseTime("").IsZero() {
		t.Error("what cannot be read should come back zero")
	}
}
