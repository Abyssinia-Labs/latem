#!/usr/bin/env bash
set -euo pipefail

# Every ci/ script sources this: it puts Homebrew Ruby on PATH and sets the
# locale, neither of which a runner step inherits. See ci/lib/env.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"

# ---------------------------------------------------------------------------
# VERIFIED END TO END, 2026-08-11.
#
# Archive → signed .ipa → TestFlight → installed on a physical iPhone, on a
# MacBook Pro M4 Max running Xcode 26.0.1. App REDACTED-APP-ID, build 1.0 (1),
# distributed to internal testers at 17:53:16. The session that did it has
# the session, including the eleven things that had to be wrong first.
#
# Nothing in this file is hypothetical any more. Where it disagrees with
# RUNNER_SPEC §18.1, this file is what ran.
#
# Optional in the environment:
#   DEVELOPMENT_TEAM               overrides the team committed in project.pbxproj
#
# Requires in the environment:
#   APP_STORE_CONNECT_KEY_ID       App Store Connect API key id
#   APP_STORE_CONNECT_ISSUER_ID    issuer id for that key
#   APP_STORE_CONNECT_KEY_P8       the .p8 private key CONTENTS (multi-line PEM)
#   RELEASE_CHANNEL                "internal" | "external"  (${input.channel})
#   RELEASE_NOTES                  changelog text           (${input.notes})
#
# Assumes ci/ios/unlock-keychain.sh and ci/ios/match.sh already ran. Their
# effects persist because the FILESYSTEM and the keychain persist across steps;
# nothing they exported does (§9).
# ---------------------------------------------------------------------------

# DEVELOPMENT_TEAM is NOT required: it is committed in project.pbxproj, which is
# the right place for it — it is a 10-character id that appears in every signed
# binary, not a secret. Setting it here overrides the project, which is what a
# fork with a different team wants.
DEVELOPMENT_TEAM="${DEVELOPMENT_TEAM:-}"
: "${APP_STORE_CONNECT_KEY_ID:?not set}"
: "${APP_STORE_CONNECT_ISSUER_ID:?not set}"
: "${APP_STORE_CONNECT_KEY_P8:?not set}"

RELEASE_CHANNEL="${RELEASE_CHANNEL:-internal}"
RELEASE_NOTES="${RELEASE_NOTES:-}"

# App Store Connect rejects a build number it has already seen for a given
# marketing version, and project.pbxproj pins CURRENT_PROJECT_VERSION = 1. So
# the first upload succeeds and every subsequent one fails — after archiving,
# exporting, and uploading, which is the expensive way to learn it.
#
# Unix time is monotonic, needs no state on disk, and stays inside the unsigned
# 32-bit range a CFBundleVersion component must fit until 2106.
#
# LATEM_BUILD_NUMBER overrides it, and under the agent it is always set: it is
# the run's own number from §22, so a build in TestFlight names the run that
# produced it (amendment A36). `latem list` prints the same number in its #
# column, which is the lookup in the other direction. The Unix-time default
# below is for running this script by hand.
#
# Moving an app from time-based numbers to run numbers means the number goes
# *down*, and Apple wants the newest build in a train to be the highest — so
# bump the marketing version, or keep overriding this for that app until its
# version changes. §18.1.
BUILD_NUMBER="${LATEM_BUILD_NUMBER:-$(date +%s)}"

PROJECT=LatemExample.xcodeproj
SCHEME=LatemExample
ARCHIVE=build/LatemExample.xcarchive
BUNDLE_ID=com.abyssinialabs.latemexample


mkdir -p build

# fastlane's --api_key_path wants a JSON file, NOT the .p8 itself.
#
# This is the single most misleading step in the pipeline. The flag is named for
# a key, the value you were given is a file called AuthKey_XXXX.p8, and handing
# it that file fails deep inside spaceship with
#   JSON::ParserError: invalid number: '-----BEGIN' at line 1 column 1
# which names JSON, names a number, and never mentions that the wrong file
# format was supplied. Verified 2026-08-11.
#
# The JSON wraps the PEM alongside the two ids. Generated with Ruby rather than
# a heredoc because the PEM is multi-line and must be escaped into a JSON string
# — a heredoc would emit literal newlines and produce invalid JSON, failing the
# same way for a different reason.
#
# Never echoed. APP_STORE_CONNECT_KEY_P8 is multi-line, which is why §16's
# redactor registers every individual LINE of a multi-line secret. Do not add
# `set -x` here: it would print a usable App Store Connect key into the log.
case "$APP_STORE_CONNECT_KEY_P8" in
  *"-----BEGIN"*) ;;
  *) echo "ERROR: APP_STORE_CONNECT_KEY_P8 does not look like a PEM." >&2
     echo "       It must be the CONTENTS of AuthKey_XXXXXXXXXX.p8, not its path." >&2
     echo '       e.g. export APP_STORE_CONNECT_KEY_P8="$(cat ~/Downloads/AuthKey_XXXXXXXXXX.p8)"' >&2
     exit 1 ;;
esac

KEY_DIR="$(mktemp -d)"
chmod 700 "$KEY_DIR"
KEY_PATH="$KEY_DIR/api_key.json"
trap 'rm -rf "$KEY_DIR"' EXIT
umask 077
ruby -rjson -e '
  File.write(ARGV[0], JSON.pretty_generate({
    "key_id"   => ENV.fetch("APP_STORE_CONNECT_KEY_ID"),
    "issuer_id"=> ENV.fetch("APP_STORE_CONNECT_ISSUER_ID"),
    "key"      => ENV.fetch("APP_STORE_CONNECT_KEY_P8"),
    "duration" => 1200,
    "in_house" => false,
  }))
' "$KEY_PATH"
chmod 600 "$KEY_PATH"

echo "--- archiving (build $BUILD_NUMBER)"
# -destination 'generic/platform=iOS' rather than -sdk iphoneos. Both appear to
# work; -destination is what modern xcodebuild wants, and mixing -sdk with a
# scheme silently picks build settings that do not match the scheme's own.
#
# Verified to produce Mach-O arm64, LC_BUILD_VERSION platform 2 (iOS device),
# CFBundleSupportedPlatforms = [iPhoneOS]. Check that rather than trusting the
# exit code: an archive that quietly contains a simulator binary exports and
# uploads and is then rejected by App Store Connect much later.
xcodebuild archive \
  -project "$PROJECT" \
  -scheme "$SCHEME" \
  -configuration Release \
  -destination 'generic/platform=iOS' \
  -archivePath "$ARCHIVE" \
  -derivedDataPath build/dd \
  ${DEVELOPMENT_TEAM:+DEVELOPMENT_TEAM="$DEVELOPMENT_TEAM"} \
  CURRENT_PROJECT_VERSION="$BUILD_NUMBER" \
  -quiet

# Read the team back out of what was actually built rather than trusting a
# variable that may be empty. This is the value the archive is signed against.
TEAM="${DEVELOPMENT_TEAM:-$(xcodebuild -project "$PROJECT" -scheme "$SCHEME" \
  -configuration Release -showBuildSettings 2>/dev/null \
  | awk -F' = ' '/ DEVELOPMENT_TEAM = /{print $2; exit}')}"
: "${TEAM:?could not determine DEVELOPMENT_TEAM}"

echo "--- exporting ipa (team $TEAM)"
# signingCertificate and provisioningProfiles are both REQUIRED under manual
# signing and both were missing from the first draft of this file. Without them
# export fails having already done the expensive part, and the message does not
# say which of the two it wanted. The profile name is match's own convention:
# "match AppStore <bundle id>" — it is not something to invent.
cat > build/ExportOptions.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>method</key><string>app-store-connect</string>
  <key>signingStyle</key><string>manual</string>
  <key>teamID</key><string>${TEAM}</string>
  <key>signingCertificate</key><string>Apple Distribution</string>
  <key>provisioningProfiles</key><dict>
    <key>${BUNDLE_ID}</key>
    <string>match AppStore ${BUNDLE_ID}</string>
  </dict>
  <key>uploadSymbols</key><true/>
</dict></plist>
PLIST

xcodebuild -exportArchive \
  -archivePath "$ARCHIVE" \
  -exportPath build/ipa \
  -exportOptionsPlist build/ExportOptions.plist

mv build/ipa/*.ipa build/app.ipa
ls -lh build/app.ipa

# Assert the thing that xcodebuild will not: that this is a device binary signed
# by a distribution certificate. Both failure modes — a simulator slice, or an
# accidentally development-signed build — export cleanly, upload cleanly, and
# are rejected by App Store Connect minutes to hours later.
VERIFY_DIR="$(mktemp -d)"
trap 'rm -rf "$KEY_DIR" "$VERIFY_DIR"' EXIT
unzip -q build/app.ipa -d "$VERIFY_DIR"
VAPP="$VERIFY_DIR/Payload/${SCHEME}.app"

codesign --verify --deep --strict "$VAPP"

# -dvv, NOT -dv. This is not a style preference: `codesign -dv` prints zero
# Authority lines, so the obvious check greps output that verbosity level never
# emits and reports "not distribution-signed" for a correctly signed bundle,
# after the archive and export have already succeeded. Verified 2026-08-11 —
# -dv gives 0 Authority lines, -dvv gives 3.
#
# Captured into a variable rather than piped into `grep -q`, because under
# `set -o pipefail` grep exits on first match, codesign takes SIGPIPE, and the
# pipeline reports failure on the very input that should pass. Two independent
# ways for this assertion to lie about a good build; neither is visible in the
# message it prints.
SIGINFO="$(codesign -dvv "$VAPP" 2>&1)"

case "$SIGINFO" in
  *"Apple Distribution"*) ;;
  *) echo "ERROR: not distribution-signed by an Apple Distribution certificate" >&2
     printf '%s\n' "$SIGINFO" >&2
     exit 1 ;;
esac

PLATFORM="$(otool -l "$VAPP/${SCHEME}" | awk '/LC_BUILD_VERSION/{f=1} f&&/platform/{print $2; exit}')"
if [ "$PLATFORM" != "2" ]; then
  echo "ERROR: LC_BUILD_VERSION platform is '$PLATFORM', want 2 (iOS device; 7 is the simulator)" >&2
  exit 1
fi

echo "    signed by: $(printf '%s\n' "$SIGINFO" | awk -F'=' '/^Authority/{print $2; exit}')"
echo "    profile:   $(security cms -D -i "$VAPP/embedded.mobileprovision" 2>/dev/null | plutil -extract Name raw - 2>/dev/null)"

echo "--- uploading to TestFlight (channel: $RELEASE_CHANNEL)"

# Branching lives here, in the shell, because jobs.yaml has no conditionals by
# design (§9). The registry says WHAT runs; the script decides HOW.
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
    # External groups require Apple's beta review, which is asynchronous and can
    # take hours to days. The run finishes when the upload finishes; it does not
    # wait for review, and it must not — a CI job that blocks on a human
    # reviewer holds a concurrency slot for a day.
    #
    # UNVERIFIED: --groups must name real external groups in App Store Connect.
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
