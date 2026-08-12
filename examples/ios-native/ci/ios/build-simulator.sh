#!/usr/bin/env bash
set -euo pipefail

# Every ci/ script sources this: it puts Homebrew Ruby on PATH and sets the
# locale, neither of which a runner step inherits. See ci/lib/env.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"

# ---------------------------------------------------------------------------
# VERIFIED. This is the one iOS path that ran end to end on 2026-08-11.
#
#   Xcode 26.0.1, MacBook Pro M4 Max, 20.2s cold, exit 0, produced
#   build/dd/Build/Products/Release-iphonesimulator/LatemExample.app
#
# It needs NO Apple Developer account, no certificate, no provisioning profile
# and no App Store Connect key, which is exactly why it is worth having: it
# proves the runner's macOS executor, the workspace, the caches, and the
# artifact collector against a real Xcode build without waiting on Apple.
#
# Use it as the smoke test when bringing up a new Mac runner. If this fails, the
# problem is the machine; if it passes and ci/ios/build.sh fails, the problem is
# credentials.
# ---------------------------------------------------------------------------

PROJECT=LatemExample.xcodeproj
SCHEME=LatemExample


mkdir -p build

# CODE_SIGNING_ALLOWED=NO is what makes this credential-free. A simulator build
# does not need a signature, but xcodebuild will still try to find one and fail
# on a machine with no identity unless told not to bother.
xcodebuild build \
  -project "$PROJECT" \
  -scheme "$SCHEME" \
  -configuration Release \
  -destination 'generic/platform=iOS Simulator' \
  -derivedDataPath build/dd \
  CODE_SIGNING_ALLOWED=NO \
  -quiet

APP="build/dd/Build/Products/Release-iphonesimulator/${SCHEME}.app"
test -d "$APP" || { echo "ERROR: no .app at $APP" >&2; exit 1; }

# Assert the bundle is real rather than trusting the exit code. xcodebuild
# returns 0 for builds that produced nothing useful more often than it should.
/usr/libexec/PlistBuddy -c "Print :CFBundleIdentifier" "$APP/Info.plist"
ls -lh "$APP/${SCHEME}"

# Package it so the agent has something to collect (§17). A .app is a directory,
# and RUNNER_SPEC §17 collects files — amendment A30 made a bare directory in
# `artifacts:` a hard E_BAD_REGISTRY error rather than something silently
# expanded, so the job declares build/*.zip and this line produces it.
( cd "$(dirname "$APP")" && zip -qry "$OLDPWD/build/app-sim.zip" "$(basename "$APP")" )
ls -lh build/app-sim.zip
