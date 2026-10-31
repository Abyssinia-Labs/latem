package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// What `latem store check` asks Apple (A65): is there an app record, which
// profile and certificate would sign a build, and what was uploaded last.
//
// Every call here goes through [Client.get], which can send nothing but a GET.
// A check runs on a machine that also builds, with a key that can change the
// account, and it must not be able to.

// APIError is Apple's refusal of a read, in Apple's own words. None of it comes
// from the credential, so it is safe to print.
type APIError struct {
	Status int
	Code   string
	Title  string
	Detail string
}

func (e *APIError) Error() string {
	switch {
	case e.Detail != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
	case e.Title != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Title)
	default:
		return fmt.Sprintf("HTTP %d", e.Status)
	}
}

// AgreementMissing reports a refusal over an unsigned agreement.
//
// Apple has no resource for agreements, so this is the only way one shows: any
// call starts failing, and only the Account Holder can clear it. The words are
// matched rather than a code, because no page of Apple's lists the code.
func (e *APIError) AgreementMissing() bool {
	text := strings.ToLower(e.Code + " " + e.Title + " " + e.Detail)
	return strings.Contains(text, "agreement")
}

// App is an app record in App Store Connect.
type App struct {
	ID       string
	Name     string
	BundleID string
}

// BundleID is a registered identifier and the profiles made for it.
type BundleID struct {
	ID         string
	Identifier string
	Profiles   []Profile
}

// Profile is a provisioning profile, without its contents.
type Profile struct {
	ID      string
	Name    string
	Type    string // IOS_APP_STORE is the one that signs for TestFlight
	State   string // ACTIVE or INVALID
	Expires time.Time
}

// Certificate is a signing certificate, without its contents.
//
// Name is what Keychain Access shows, and Type is what Apple calls it: EAS's
// certificate is named iPhone Distribution and has the type IOS_DISTRIBUTION,
// fastlane's is named Apple Distribution and has the type DISTRIBUTION. Either
// signs for the App Store.
type Certificate struct {
	ID      string
	Name    string
	Type    string
	Expires time.Time
}

// Build is an upload to App Store Connect.
type Build struct {
	Version  string
	State    string // PROCESSING, FAILED, INVALID or VALID
	Uploaded time.Time
	Expires  time.Time
	Expired  bool
}

// AppByBundleID finds the app record for a bundle ID.
//
// Apple's filter is not promised to be exact, so the answer is matched here:
// com.example.app must not be satisfied by com.example.app.widget.
func (c *Client) AppByBundleID(ctx context.Context, token, bundleID string) (App, bool, error) {
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name     string `json:"name"`
				BundleID string `json:"bundleId"`
			} `json:"attributes"`
		} `json:"data"`
	}
	query := url.Values{
		"filter[bundleId]": {bundleID},
		"fields[apps]":     {"name,bundleId"},
		"limit":            {"200"},
	}
	if err := c.get(ctx, token, "/v1/apps", query, &body); err != nil {
		return App{}, false, err
	}
	for _, a := range body.Data {
		if a.Attributes.BundleID == bundleID {
			return App{ID: a.ID, Name: a.Attributes.Name, BundleID: bundleID}, true, nil
		}
	}
	return App{}, false, nil
}

// BundleIDByIdentifier finds a registered bundle ID and its profiles.
func (c *Client) BundleIDByIdentifier(ctx context.Context, token, identifier string) (BundleID, bool, error) {
	type ref struct {
		ID string `json:"id"`
	}
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Identifier string `json:"identifier"`
			} `json:"attributes"`
			Relationships struct {
				Profiles struct {
					Data []ref `json:"data"`
				} `json:"profiles"`
			} `json:"relationships"`
		} `json:"data"`
		Included []struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			Attributes struct {
				Name           string `json:"name"`
				ProfileType    string `json:"profileType"`
				ProfileState   string `json:"profileState"`
				ExpirationDate string `json:"expirationDate"`
			} `json:"attributes"`
		} `json:"included"`
	}
	query := url.Values{
		"filter[identifier]": {identifier},
		"fields[bundleIds]":  {"identifier,profiles"},
		// Without profileContent: nothing here reads a profile's contents.
		"fields[profiles]": {"name,profileType,profileState,expirationDate"},
		"include":          {"profiles"},
		"limit":            {"200"},
		"limit[profiles]":  {"50"},
	}
	if err := c.get(ctx, token, "/v1/bundleIds", query, &body); err != nil {
		return BundleID{}, false, err
	}
	for _, b := range body.Data {
		if b.Attributes.Identifier != identifier {
			continue
		}
		out := BundleID{ID: b.ID, Identifier: identifier}
		mine := make(map[string]bool, len(b.Relationships.Profiles.Data))
		for _, r := range b.Relationships.Profiles.Data {
			mine[r.ID] = true
		}
		for _, inc := range body.Included {
			if inc.Type != "profiles" || !mine[inc.ID] {
				continue
			}
			out.Profiles = append(out.Profiles, Profile{
				ID: inc.ID, Name: inc.Attributes.Name, Type: inc.Attributes.ProfileType,
				State: inc.Attributes.ProfileState, Expires: parseTime(inc.Attributes.ExpirationDate),
			})
		}
		return out, true, nil
	}
	return BundleID{}, false, nil
}

// ProfileCertificates lists the certificates a profile can be signed with.
func (c *Client) ProfileCertificates(ctx context.Context, token, profileID string) ([]Certificate, error) {
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name            string `json:"name"`
				CertificateType string `json:"certificateType"`
				ExpirationDate  string `json:"expirationDate"`
			} `json:"attributes"`
		} `json:"data"`
	}
	query := url.Values{
		"fields[certificates]": {"name,certificateType,expirationDate"},
		"limit":                {"50"},
	}
	path := "/v1/profiles/" + url.PathEscape(profileID) + "/certificates"
	if err := c.get(ctx, token, path, query, &body); err != nil {
		return nil, err
	}
	out := make([]Certificate, 0, len(body.Data))
	for _, cert := range body.Data {
		out = append(out, Certificate{
			ID: cert.ID, Name: cert.Attributes.Name, Type: cert.Attributes.CertificateType,
			Expires: parseTime(cert.Attributes.ExpirationDate),
		})
	}
	return out, nil
}

// LatestBuild is the newest upload for an app.
func (c *Client) LatestBuild(ctx context.Context, token, appID string) (Build, bool, error) {
	var body struct {
		Data []struct {
			Attributes struct {
				Version         string `json:"version"`
				ProcessingState string `json:"processingState"`
				UploadedDate    string `json:"uploadedDate"`
				ExpirationDate  string `json:"expirationDate"`
				Expired         bool   `json:"expired"`
			} `json:"attributes"`
		} `json:"data"`
	}
	query := url.Values{
		"filter[app]":    {appID},
		"sort":           {"-uploadedDate"},
		"limit":          {"1"},
		"fields[builds]": {"version,processingState,uploadedDate,expirationDate,expired"},
	}
	if err := c.get(ctx, token, "/v1/builds", query, &body); err != nil {
		return Build{}, false, err
	}
	if len(body.Data) == 0 {
		return Build{}, false, nil
	}
	a := body.Data[0].Attributes
	return Build{
		Version: a.Version, State: a.ProcessingState, Expired: a.Expired,
		Uploaded: parseTime(a.UploadedDate), Expires: parseTime(a.ExpirationDate),
	}, true, nil
}

// get reads one resource. It is the only way a read reaches Apple, and it
// takes no method: what `latem store check` cannot ask for, it cannot change.
func (c *Client) get(ctx context.Context, token, path string, query url.Values, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body := io.LimitReader(resp.Body, 4<<20)
	if resp.StatusCode != http.StatusOK {
		return refusal(resp.StatusCode, body)
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// refusal reads the first of Apple's errors, which is the one that matters;
// the rest repeat it per field.
func refusal(status int, body io.Reader) error {
	var doc struct {
		Errors []struct {
			Code   string `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	out := &APIError{Status: status}
	if err := json.NewDecoder(body).Decode(&doc); err != nil || len(doc.Errors) == 0 {
		return out
	}
	first := doc.Errors[0]
	out.Code, out.Title, out.Detail = first.Code, first.Title, first.Detail
	return out
}

// AsAPIError is errors.As for the one error type a caller branches on.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// parseTime reads Apple's dates. A date it cannot read comes back zero, which
// a caller reports as "no date" rather than failing the whole answer.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
