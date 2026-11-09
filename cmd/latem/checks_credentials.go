package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/appstore"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// checkCredentials checks that an App Store Connect credential *set* agrees
// with itself (amendment A35).
//
// Every other secret check in `doctor` asks whether a name resolves, which is
// the right question for a secret whose only property is being present. It is
// the wrong question here: each of the three values can be individually valid
// while the set is unusable, and `fastlane pilot upload` reports that, a wrong
// issuer id, and a revoked key identically — after archiving, exporting and
// uploading. Two minutes per guess, against three independent values, one of
// which is a PEM nobody can eyeball.
//
// Runs only under --secrets, with the rest of the checks that resolve values.
// It prints key ids, fingerprints and bundle ids; it never prints key material
// (INV-4).
func (d *doctor) checkCredentials(ctx context.Context) []check {
	chain, err := d.newSecrets(secrets.Options{
		FilePath:  d.cfg.Paths.SecretsEnv,
		Commands:  d.cfg.Secrets.Commands,
		OpenVault: d.cfg.Secrets.OpenVault,
	})
	if err != nil {
		return nil // checkSecrets has already reported the chain itself
	}

	creds, resolved, missing, unreadable := resolveAppStore(ctx, chain)
	defer creds.zero()

	switch {
	case len(resolved) == 0 && len(unreadable) == 0:
		// No iOS signing credentials on this machine. Not a fault: a Linux
		// runner, or a Mac that only builds simulator smoke jobs.
		return nil
	case len(missing) > 0 || len(unreadable) > 0:
		return []check{fail("secret.asc.set", "App Store Connect set",
			appStoreSetDetail(resolved, missing, unreadable),
			fmt.Sprintf("# add the missing names to %s", d.cfg.Paths.SecretsEnv)).
			withNotes("All three are needed together. A partial set fails at upload with the same " +
				"generic authentication error as a wrong one, having first spent the build.")}
	}

	out := d.appStoreShape(creds)
	if anyFailed(out) {
		return append(out, skip("secret.asc.auth", "App Store Connect auth",
			"not contacting Apple while the values above are wrong"))
	}
	return append(out, d.appStoreAuth(ctx, creds.credentials())...)
}

// appStoreSetDetail states each name once, under the one heading that is true
// of it.
//
// The three lists are disjoint, which is the fix: a name whose provider errored
// used to be appended to both "present" and "missing" so that a broken backend
// could not make the set look complete. The intent was right and the sentence
// it produced was not — with one unparseable secrets.env every name appeared on
// both sides, and the check read "A, B, C resolve, A, B, C do not".
func appStoreSetDetail(resolved, missing, unreadable []string) string {
	var parts []string
	if len(resolved) > 0 {
		parts = append(parts, fmt.Sprintf("%s resolve", strings.Join(resolved, ", ")))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%s do not", strings.Join(missing, ", ")))
	}
	if len(unreadable) > 0 {
		parts = append(parts, fmt.Sprintf("%s could not be read (see above)", strings.Join(unreadable, ", ")))
	}
	return strings.Join(parts, "; ")
}

// appStoreShape is everything knowable without a network: the two identifiers'
// shapes, whether the key parses, and — the A35 finding itself — whether the
// key material on disk agrees with the key id the environment names.
func (d *doctor) appStoreShape(creds appStoreSecrets) []check {
	var out []check

	if appstore.ValidKeyID(creds.keyID) {
		out = append(out, pass("secret.asc.keyid", "App Store Connect key id", creds.keyID))
	} else {
		out = append(out, fail("secret.asc.keyid", "App Store Connect key id",
			fmt.Sprintf("%q is not ten uppercase alphanumerics", creds.keyID),
			"# it is the XXXXXXXXXX in the AuthKey_XXXXXXXXXX.p8 you downloaded"))
	}

	if appstore.ValidIssuerID(creds.issuerID) {
		out = append(out, pass("secret.asc.issuer", "App Store Connect issuer id", creds.issuerID))
	} else {
		out = append(out, fail("secret.asc.issuer", "App Store Connect issuer id",
			fmt.Sprintf("%q is not a UUID", creds.issuerID),
			"# App Store Connect → Users and Access → Integrations → App Store Connect API").
			withNotes("The issuer id sits above the key table and is one per team, not per key. "+
				"The common mistake is pasting the key id, a team id, or an App ID here — none of "+
				"which are UUIDs, which is the only reason this is checkable."))
	}

	key, err := appstore.ParseKey(creds.p8)
	if err != nil {
		return append(out, fail("secret.asc.key", "App Store Connect private key", err.Error(),
			"# the secret must be the CONTENTS of the .p8, beginning -----BEGIN PRIVATE KEY-----").
			withNotes("A .p8 is 257 bytes and one PEM block. A path to the file, a base64 of the "+
				"whole file, or a copy that lost its header are all values that look plausible in a "+
				"secrets file and cannot sign a token."))
	}
	fingerprint := appstore.Fingerprint(&key.PublicKey)
	out = append(out, pass("secret.asc.key", "App Store Connect private key",
		fmt.Sprintf("EC P-256, public fingerprint %s", fingerprint)))

	// Which .p8 did this material actually come from? Apple names every
	// download AuthKey_<KEY_ID>.p8, so a file on disk carries its own key id
	// and the fingerprint says whether it is this key.
	files := appstore.ScanKeyFiles(appstore.DefaultKeyDirs(d.sys.Home()))
	file, found, agrees := appstore.MatchOnDisk(creds.credentials(), files)
	switch {
	case found && agrees:
		out = append(out, pass("secret.asc.material", "key id agreement",
			fmt.Sprintf("matches AuthKey_%s.p8, and the key id agrees", file.KeyID)))
	case found:
		out = append(out, fail("secret.asc.material", "key id agreement",
			fmt.Sprintf("%s names key %s, but the key material is from AuthKey_%s.p8",
				appstore.SecretKeyID, creds.keyID, file.KeyID),
			fmt.Sprintf("# set %s=%s, having confirmed that is the key you created",
				appstore.SecretKeyID, file.KeyID)).
			withNotes(fmt.Sprintf("The token would claim kid=%s while being signed by %s. Apple "+
				"rejects that with a 401 that names neither half.", creds.keyID, file.KeyID),
				"Found at "+file.Path+". Anyone who has ever made a second key — an APNs key, an "+
					"older CI key — has several of these files, all 257 bytes, all valid."))
	case len(files) > 0:
		out = append(out, warn("secret.asc.material", "key id agreement",
			fmt.Sprintf("the key material matches none of the %d AuthKey_*.p8 on this machine", len(files))).
			withNotes("Which is fine if the secret was pasted from elsewhere — it only means this "+
				"check cannot confirm the key id from a filename. Apple's answer below is the "+
				"authoritative one."))
	default:
		out = append(out, skip("secret.asc.material", "key id agreement",
			"no AuthKey_*.p8 on this machine to compare against"))
	}
	return out
}

// appStoreAuth asks Apple, which is the only authority on whether a key id and
// a private key are halves of the same key.
//
// Both token forms are tried because Apple issues two kinds and they are not
// interchangeable in the payload: a team key carries `iss`, an individual key
// omits it and carries `sub`. Using the wrong shape returns the same 401 a
// wrong credential does, and an individual key authenticating here while
// fastlane keeps failing is a full afternoon if nothing says so.
func (d *doctor) appStoreAuth(ctx context.Context, creds appstore.Credentials) []check {
	const id, title = "secret.asc.auth", "App Store Connect auth"

	client := d.appStoreClient()
	now := d.now()

	var firstErr error
	for _, form := range []appstore.TokenForm{appstore.TeamKey, appstore.IndividualKey} {
		token, err := appstore.Token(creds, form, now)
		if err != nil {
			return []check{fail(id, title, "could not mint a token: "+err.Error())}
		}
		res, err := client.Apps(ctx, token)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		switch {
		case res.Status == http.StatusOK && form == appstore.IndividualKey:
			return []check{fail(id, title,
				"authenticated as an INDIVIDUAL key, which fastlane cannot use",
				"# App Store Connect → Users and Access → Integrations → App Store Connect API",
				"# → Team Keys tab → +, role App Manager").
				withNotes("fastlane's spaceship builds only the team-key token, so `pilot` will keep " +
					"returning 401 however correct this key is. The individual key can stay; it is " +
					"simply not usable here.")}
		case res.Status == http.StatusOK:
			c := pass(id, title, fmt.Sprintf("Apple accepted this key — %d app(s) visible", len(res.BundleIDs)))
			if len(res.BundleIDs) == 0 {
				c = warn(id, title, "Apple accepted this key, and it can see no apps",
					"# App Store Connect → Apps → +, or widen this key's role").
					withNotes("Authentication is not the same as visibility: an upload fails for an " +
						"app this key cannot see, after the build has been paid for.")
			} else {
				c = c.withNotes("visible: " + strings.Join(res.BundleIDs, ", "))
			}
			return []check{c}
		case res.Status == http.StatusForbidden:
			return []check{fail(id, title, "403 — authentication worked, authorisation did not",
				"# raise this key's role in App Store Connect to App Manager").
				withNotes("TestFlight uploads need App Manager; Developer is not sufficient.")}
		case res.Status != http.StatusUnauthorized:
			return []check{warn(id, title, fmt.Sprintf("Apple answered HTTP %d", res.Status))}
		}
		// 401 on the team form falls through to the individual form.
	}

	if firstErr != nil {
		return []check{skip(id, title, "could not reach Apple: "+firstErr.Error())}
	}
	return []check{fail(id, title, "401 — the values are well formed and Apple rejected them",
		"# check, in this order: the issuer id's team, whether the key was revoked,",
		"# and whether key id and .p8 are from the same key (see key id agreement above)").
		withNotes("A 401 for both token forms means the set does not describe a usable key. The " +
			"three causes are indistinguishable from Apple's answer, which is why the checks above " +
			"exist: they narrow it before this one runs.")}
}

// appStoreSecrets holds the three resolved values.
//
// [secrets.Value] is []byte so it can be zeroed (§15, F10). The two
// identifiers become strings because they are printed; the key never does.
type appStoreSecrets struct {
	keyID    string
	issuerID string
	p8       []byte

	held []secrets.Value
}

func (a *appStoreSecrets) credentials() appstore.Credentials {
	return appstore.Credentials{KeyID: a.keyID, IssuerID: a.issuerID, P8: a.p8}
}

// zero releases every value this check resolved.
func (a *appStoreSecrets) zero() {
	for _, v := range a.held {
		v.Zero()
	}
	a.held, a.p8 = nil, nil
}

// resolveAppStore resolves the trio, reporting which names were found and
// which were not.
//
// A name that no provider holds is "not present" rather than an error: a
// machine with no iOS credentials is a supported machine, and the difference
// between all-three-absent and one-absent is the whole point of the caller's
// first switch.
func resolveAppStore(ctx context.Context, chain secretResolver) (out appStoreSecrets, resolved, missing, unreadable []string) {
	for _, name := range []string{appstore.SecretKeyID, appstore.SecretIssuerID, appstore.SecretP8} {
		value, err := chain.Resolve(ctx, name)
		if err != nil || len(value) == 0 {
			value.Zero()
			if err != nil && !errors.Is(err, secrets.ErrNotFound) {
				// A backend that is broken rather than empty — a locked
				// keychain, or a secrets.env that does not parse. Its own
				// category: it is neither held nor absent, and counting it as
				// both produced "A, B, C resolve, A, B, C do not".
				unreadable = append(unreadable, name)
				continue
			}
			missing = append(missing, name)
			continue
		}
		resolved = append(resolved, name)
		out.held = append(out.held, value)
		switch name {
		case appstore.SecretKeyID:
			out.keyID = strings.TrimSpace(string(value))
		case appstore.SecretIssuerID:
			out.issuerID = strings.TrimSpace(string(value))
		case appstore.SecretP8:
			out.p8 = value
		}
	}
	return out, resolved, missing, unreadable
}

func anyFailed(checks []check) bool {
	for _, c := range checks {
		if c.Level == levelFail {
			return true
		}
	}
	return false
}

// appStoreClient is the injected client, or a real one talking to Apple.
//
// Nil-safe rather than requiring every construction site to fill it: a doctor
// assembled for one check should not have to know which other checks make
// network calls.
func (d *doctor) appStoreClient() *appstore.Client {
	if d.newAppStore != nil {
		return d.newAppStore()
	}
	return &appstore.Client{}
}

// now is injectable so a test is not signing tokens against the wall clock.
func (d *doctor) now() time.Time {
	if d.clock != nil {
		return d.clock()
	}
	return time.Now()
}
