#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# Written from RUNNER_SPEC §18.1 as M0 scaffolding. No machine has run it, no
# .ipa has been produced by it, and nothing has reached TestFlight. The two
# core commands (`eas build --local`, `fastlane pilot upload`) are from §18.1
# verbatim; the API-key handling and branching around them are drafts.
# Replace this file with the commands that actually worked.
#
# Requires in the environment:
#   APP_STORE_CONNECT_KEY_ID       App Store Connect API key id
#   APP_STORE_CONNECT_ISSUER_ID    issuer id for that key
#   APP_STORE_CONNECT_KEY_P8       the .p8 private key CONTENTS (multi-line PEM)
#   RELEASE_CHANNEL                "internal" | "external"  (from ${input.channel})
#   RELEASE_NOTES                  changelog text           (from ${input.notes})
#
# Assumes ci/ios/unlock-keychain.sh and ci/ios/match.sh already ran in this run.
# Their effects persist because the FILESYSTEM and the keychain persist across
# steps; nothing they exported does (§9).
# ---------------------------------------------------------------------------

: "${APP_STORE_CONNECT_KEY_ID:?not set}"
: "${APP_STORE_CONNECT_ISSUER_ID:?not set}"
: "${APP_STORE_CONNECT_KEY_P8:?not set}"

RELEASE_CHANNEL="${RELEASE_CHANNEL:-internal}"
RELEASE_NOTES="${RELEASE_NOTES:-}"

mkdir -p build

# fastlane wants the key as a file. Write it to a private temp file and remove
# it on every exit path — including failure, which is when people forget.
#
# Note what this means for logging: APP_STORE_CONNECT_KEY_P8 is multi-line, and
# §16's redactor registers every individual LINE of a multi-line secret for
# exactly this reason. A redactor that only knows the whole value would let each
# base64 body line through untouched, and `set -x` in this script would print a
# usable App Store Connect key into the run log. Do not add `set -x` here.
KEY_DIR="$(mktemp -d)"
chmod 700 "$KEY_DIR"
KEY_PATH="$KEY_DIR/AuthKey_${APP_STORE_CONNECT_KEY_ID}.p8"
trap 'rm -rf "$KEY_DIR"' EXIT
umask 077
printf '%s\n' "$APP_STORE_CONNECT_KEY_P8" > "$KEY_PATH"

echo "--- building ipa locally"
# --local runs the same EAS pipeline on this machine instead of Expo's builders.
# It is a supported flag, invoked as an ordinary command; if Expo changes it the
# blast radius is this one script and not the agent (§18.3).
#
# --non-interactive is load-bearing. Without it EAS prompts — for credentials,
# for confirmation — and an unattended runner hangs at a prompt nobody can see
# until the 70-minute step timeout kills it (§18.1 failure table).
#
# EAS must not manage credentials: match already installed them, and letting EAS
# fetch its own would race with match and can create new certificates.
export EXPO_NO_CAPABILITY_SYNC=1
npx eas build \
  --platform ios \
  --profile production \
  --local \
  --non-interactive \
  --output "build/app.ipa"

ls -lh build/app.ipa

echo "--- uploading to TestFlight (channel: $RELEASE_CHANNEL)"

# Branching lives here, in the shell, because jobs.yaml has no conditionals by
# design (§9). This is N1 working as intended: the registry says WHAT runs, the
# script decides HOW.
common=(
  --ipa build/app.ipa
  --api_key_path "$KEY_PATH"
  --changelog "$RELEASE_NOTES"
  --skip_waiting_for_build_processing false
)

case "$RELEASE_CHANNEL" in
  internal)
    # Internal testers only. No Apple beta review, available in minutes.
    bundle exec fastlane pilot upload "${common[@]}" --distribute_external false
    ;;
  external)
    # External groups require Apple's beta review, which is asynchronous and
    # can take hours to days. The run finishes when the upload finishes; it
    # does not wait for review, and it must not — a CI job that blocks on a
    # human reviewer holds a concurrency slot for a day.
    #
    # UNVERIFIED: --groups must name real external groups in App Store Connect.
    # Fill in the real names during M0.
    bundle exec fastlane pilot upload "${common[@]}" \
      --distribute_external true \
      --groups "External Testers"
    ;;
  *)
    echo "ERROR: unknown RELEASE_CHANNEL '$RELEASE_CHANNEL'" >&2
    exit 1
    ;;
esac

echo "--- uploaded"
# The .ipa stays in build/ for the agent to collect as an artifact (§17). Do not
# delete it here; `collecting` runs after this step and globs build/*.ipa.
