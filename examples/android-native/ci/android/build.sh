#!/usr/bin/env bash
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"

# ---------------------------------------------------------------------------
# VERIFIED END TO END, 2026-08-11.
#
# Signed .aab → Play internal track at 21:07:34, signed by
# CN=Latem Upload Key, O=Abyssinia Labs. The first upload went through the API
# with no manual bundle needed first, contradicting the widely-repeated advice
# that a draft app must receive its first bundle through the Play Console UI.
#
#
# Requires in the environment:
#   ANDROID_KEYSTORE_B64          upload keystore, base64 encoded
#   ANDROID_KEYSTORE_PASSWORD     its store password
#   ANDROID_KEY_ALIAS             key alias inside the keystore
#   ANDROID_KEY_PASSWORD          that key's password
#   PLAY_SERVICE_ACCOUNT_JSON     service account credentials, JSON contents
#   RELEASE_TRACK                 internal|alpha|beta  (${input.track})
#   RELEASE_NOTES                 changelog text       (${input.notes})
# ---------------------------------------------------------------------------

: "${ANDROID_KEYSTORE_B64:?not set}"
: "${ANDROID_KEYSTORE_PASSWORD:?not set}"
: "${ANDROID_KEY_ALIAS:?not set}"
: "${ANDROID_KEY_PASSWORD:?not set}"
: "${PLAY_SERVICE_ACCOUNT_JSON:?not set}"

RELEASE_TRACK="${RELEASE_TRACK:-internal}"
RELEASE_NOTES="${RELEASE_NOTES:-}"

# Play rejects a versionCode it has already accepted, so a pinned value uploads
# exactly once and then fails forever — the same trap as CFBundleVersion on iOS
# (amendment A36). Under the agent LATEM_BUILD_NUMBER is the run's own number
# from §22, which makes a release traceable to the run that built it; the
# Unix-time default is for running this by hand, and it fits Play's 2100000000
# ceiling until 2036.
#
# Play enforces strictly increasing versionCodes, so an app that has already
# uploaded time-based ones needs the override kept until it is retired.
VERSION_CODE="${LATEM_BUILD_NUMBER:-$(date +%s)}"

# The keystore is a binary and arrives base64 so it can travel as one line
# through §15's secret chain. Written to a private temp dir, removed on every
# exit path including failure.
#
# Never echoed: the passwords below are registered with §16's redactor, but a
# `set -x` here would print the decoded keystore path and the base64 itself.
SECRET_DIR="$(mktemp -d)"
chmod 700 "$SECRET_DIR"
trap 'rm -rf "$SECRET_DIR"' EXIT
umask 077

export ANDROID_KEYSTORE_PATH="$SECRET_DIR/upload.keystore"
printf '%s' "$ANDROID_KEYSTORE_B64" | base64 --decode > "$ANDROID_KEYSTORE_PATH"

# Fail here rather than inside Gradle, whose error for a corrupt keystore is
# "Failed to read key from store" and names neither base64 nor the password.
"$JAVA_HOME/bin/keytool" -list -keystore "$ANDROID_KEYSTORE_PATH" \
  -storepass "$ANDROID_KEYSTORE_PASSWORD" -alias "$ANDROID_KEY_ALIAS" >/dev/null 2>&1 || {
  echo "ERROR: keystore did not open with the supplied password and alias." >&2
  echo "       Either ANDROID_KEYSTORE_B64 is not valid base64 of a keystore," >&2
  echo "       ANDROID_KEYSTORE_PASSWORD is wrong, or ANDROID_KEY_ALIAS names" >&2
  echo "       a key that is not in it. keytool -list would show the aliases." >&2
  exit 1
}

echo "--- building signed bundle (versionCode $VERSION_CODE)"
./gradlew :app:bundleRelease --no-daemon -PlatemVersionCode="$VERSION_CODE"

AAB=app/build/outputs/bundle/release/app-release.aab
test -f "$AAB" || { echo "ERROR: no bundle at $AAB" >&2; exit 1; }
mkdir -p build && cp "$AAB" build/app.aab
ls -lh build/app.aab

# Assert it is actually signed. An unsigned bundle builds happily — that is how
# the smoke test works — and Play rejects it after the upload, which is the
# expensive place to find out.
#
# jarsigner, not a grep over `unzip -l`. An .aab is JAR-signed, so jarsigner is
# the tool that actually knows, and it reports the certificate rather than the
# mere presence of a file whose name ends in .RSA.
#
# The first version of this check grepped the zip listing and reported a
# correctly signed bundle as unsigned, once, unreproducibly — which is the worst
# kind of assertion: it cost a debugging session and left no evidence behind. So
# this one prints what it saw when it fails. An assertion that says only "no"
# is barely better than no assertion at all.
SIGCHECK="$("$JAVA_HOME/bin/jarsigner" -verify -verbose:summary build/app.aab 2>&1)" || true
case "$SIGCHECK" in
  *"jar verified"*) ;;
  *)
    echo "ERROR: build/app.aab is not signed." >&2
    echo "       ANDROID_KEYSTORE_PATH was probably not visible to Gradle." >&2
    echo "       jarsigner said:" >&2
    # pipefail-safe: diagnostics on the way to a hard failure. A SIGPIPE here
    # would replace the error being reported with an error about reporting it.
    printf '%s\n' "$SIGCHECK" | head -20 | sed 's/^/         /' >&2 || true
    echo "       zip entries under META-INF:" >&2
    unzip -l build/app.aab > /tmp/aab-listing.$$ 2>&1 || true
    grep -i "META-INF" /tmp/aab-listing.$$ | head -10 | sed 's/^/         /' >&2 || true
    rm -f /tmp/aab-listing.$$
    exit 1 ;;
esac
echo "    signed: yes  ($("$JAVA_HOME/bin/keytool" -printcert -jarfile build/app.aab 2>/dev/null | awk -F': ' '/Owner:/{print $2; exit}'))"

echo "--- uploading to Play ($RELEASE_TRACK)"
KEY_JSON="$SECRET_DIR/play.json"
printf '%s' "$PLAY_SERVICE_ACCOUNT_JSON" > "$KEY_JSON"

# fastlane ships a validator for exactly this credential. Use it before the
# upload rather than after: `supply` reports an unusable service account as a
# permissions error at the end of a 45-minute job, and this answers in seconds.
#
# It is the Play equivalent of examples/ios-native/ci/ios/check-credentials.sh,
# except it already existed — worth knowing before writing another one.
bundle exec fastlane run validate_play_store_json_key json_key:"$KEY_JSON" || {
  echo "ERROR: the Play service account JSON was rejected." >&2
  echo "       Common causes, in order:" >&2
  echo "         1. the service account was created but never INVITED in Play" >&2
  echo "            Console → Users and permissions (a Cloud key alone grants" >&2
  echo "            nothing)" >&2
  echo "         2. the Google Play Android Developer API is not enabled on the" >&2
  echo "            linked Cloud project" >&2
  echo "         3. permissions were granted minutes ago and have not propagated" >&2
  exit 1
}

# internal publishes; every other track uploads as a draft for a human to
# promote.
#
# The asymmetry is the point. The internal track is capped at 100 named testers,
# has no Google review, and is reached only by an opt-in link you hand out — so
# a run that lands there directly is about as low-stakes as publishing gets, and
# leaving it as a draft just adds a manual step to every build. alpha and beta
# are audiences you do not individually know, so those stay behind a human.
#
# This is the one place in the pipeline where the runner does something the
# outside world can see, so it is written to be read at a glance rather than
# inferred from a flag.
case "$RELEASE_TRACK" in
  internal) RELEASE_STATUS=completed ;;
  *)        RELEASE_STATUS=draft ;;
esac
echo "    track $RELEASE_TRACK → release_status $RELEASE_STATUS"

# RELEASE_NOTES was accepted and silently discarded until now: `supply` has no
# --changelog flag (unlike pilot), it reads changelogs from a metadata tree keyed
# by versionCode. Written here, removed by the trap — it is per-run output, not
# repository content.
if [ -n "$RELEASE_NOTES" ]; then
  CHANGELOG_DIR="fastlane/metadata/android/en-US/changelogs"
  mkdir -p "$CHANGELOG_DIR"
  printf '%s\n' "$RELEASE_NOTES" > "$CHANGELOG_DIR/${VERSION_CODE}.txt"
  trap 'rm -rf "$SECRET_DIR" "fastlane/metadata"' EXIT
fi

# The store listing — title, description, screenshots — is deliberately NOT
# uploaded. Those are owned by whoever writes them in the Play Console, and a CI
# job that pushes an empty local metadata tree over them is a well-known way to
# blank a live listing. Only the binary and its changelog go up.
bundle exec fastlane supply \
  --aab build/app.aab \
  --track "$RELEASE_TRACK" \
  --json_key "$KEY_JSON" \
  --package_name com.abyssinialabs.latemexample \
  --release_status "$RELEASE_STATUS" \
  --skip_upload_apk true \
  --skip_upload_metadata true \
  --skip_upload_images true \
  --skip_upload_screenshots true

echo "--- uploaded"
