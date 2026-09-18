#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# Written from RUNNER_SPEC §18.2 as M0 scaffolding. No machine has run it, no
# .aab has been produced, and nothing has reached Play internal testing. The
# two core commands are from §18.2 verbatim; the keystore and service-account
# handling around them are drafts. Replace with what worked.
#
# Requires in the environment:
#   ANDROID_KEYSTORE_B64         base64 of the release keystore (.jks)
#   ANDROID_KEYSTORE_PASSWORD    keystore password
#   ANDROID_KEY_ALIAS            signing key alias inside the keystore
#   ANDROID_KEY_PASSWORD         password for that alias
#   PLAY_SERVICE_ACCOUNT_JSON    Play Developer API service account JSON
#   RELEASE_TRACK                "internal" | "alpha" | "beta"  (${input.track})
#   RELEASE_NOTES                changelog text                 (${input.notes})
# ---------------------------------------------------------------------------

: "${ANDROID_KEYSTORE_B64:?not set}"
: "${ANDROID_KEYSTORE_PASSWORD:?not set}"
: "${ANDROID_KEY_ALIAS:?not set}"
: "${ANDROID_KEY_PASSWORD:?not set}"
: "${PLAY_SERVICE_ACCOUNT_JSON:?not set}"

RELEASE_TRACK="${RELEASE_TRACK:-internal}"
RELEASE_NOTES="${RELEASE_NOTES:-}"

mkdir -p build

# Both credentials have to exist as files on disk for the tooling to read them.
# Keep them in one mode-0700 temp directory with a single trap, so there is one
# cleanup path rather than two that can drift.
SECRET_DIR="$(mktemp -d)"
chmod 700 "$SECRET_DIR"
trap 'rm -rf "$SECRET_DIR"' EXIT
umask 077

KEYSTORE_PATH="$SECRET_DIR/release.jks"
PLAY_JSON_PATH="$SECRET_DIR/play-service-account.json"

# base64 -d on GNU coreutils, -D on BSD/macOS. This job targets the Linux
# container runner, but the decode is written portably because the same script
# runs by hand on a Mac during M0.
# Captured and matched in the shell, no pipeline (amendment A37): piped into
# `grep -q` this reports a failure when the help text matched, which silently
# picks the BSD branch on a GNU box.
base64_help=$(base64 --help 2>&1 || true)
if [[ $base64_help == *"-d,"* ]]; then
  printf '%s' "$ANDROID_KEYSTORE_B64" | base64 -d > "$KEYSTORE_PATH"
else
  printf '%s' "$ANDROID_KEYSTORE_B64" | base64 -D > "$KEYSTORE_PATH"
fi

printf '%s' "$PLAY_SERVICE_ACCOUNT_JSON" > "$PLAY_JSON_PATH"

# Verify the keystore decoded into something real before spending 40 minutes
# on a build that cannot be signed. `keytool -list` fails on a truncated or
# wrongly-encoded file, which is the common outcome of a copy-paste that lost a
# trailing newline.
keytool -list -keystore "$KEYSTORE_PATH" \
  -storepass "$ANDROID_KEYSTORE_PASSWORD" \
  -alias "$ANDROID_KEY_ALIAS" >/dev/null

echo "--- building aab locally"
# EAS reads these from the environment via the credentials source configured in
# eas.json / credentials.json. UNVERIFIED: confirm during M0 which of these the
# project's eas.json actually consumes — Expo has changed this surface, and a
# variable that is set but unread produces an unsigned build rather than an
# error.
export ANDROID_KEYSTORE_PATH="$KEYSTORE_PATH"
export ANDROID_KEYSTORE_PASSWORD ANDROID_KEY_ALIAS ANDROID_KEY_PASSWORD

# The Gradle cache is the largest win on this platform (§18.2) and is mounted,
# not restored (§14). Point Gradle at the mount; its default is $HOME/.gradle
# which may not be the mounted path in every base image.
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-/home/runner/.gradle}"

npx eas build \
  --platform android \
  --profile production \
  --local \
  --non-interactive \
  --output build/app.aab

ls -lh build/app.aab

echo "--- uploading to Play track: $RELEASE_TRACK"
# supply pushes to a track; it does not promote or roll out to production. The
# job deliberately offers only internal/alpha/beta as enum values — a production
# rollout is a human decision made in the Play console, not a CI input.
bundle exec fastlane supply \
  --aab build/app.aab \
  --track "$RELEASE_TRACK" \
  --json_key "$PLAY_JSON_PATH" \
  --skip_upload_metadata true \
  --skip_upload_images true \
  --skip_upload_screenshots true \
  --release_status draft

echo "--- uploaded"
# UNVERIFIED: --release_status draft means the build lands but is not released
# to testers until someone clicks in the console. For internal testing you may
# want "completed" instead. Decide during M0 and write the answer down.

# The .aab stays in build/ for artifact collection (§17).
