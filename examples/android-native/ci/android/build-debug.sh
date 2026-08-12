#!/usr/bin/env bash
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"

# ---------------------------------------------------------------------------
# VERIFIED 2026-08-11. The credential-free Android smoke test.
#
#   ./gradlew :app:assembleDebug  → 14.6s → app-debug.apk (853K)
#
# Needs no Google account, no keystore, no service account. Use it to prove a
# new Mac or Linux runner before any credential exists: if this fails the
# problem is the machine, and if it passes while ci/android/build.sh fails the
# problem is credentials.
# ---------------------------------------------------------------------------

./gradlew :app:assembleDebug --no-daemon

APK=app/build/outputs/apk/debug/app-debug.apk
test -f "$APK" || { echo "ERROR: no apk at $APK" >&2; exit 1; }

# Assert the contents rather than the exit code. aapt2 lives under a versioned
# build-tools directory, so pick the newest rather than pinning one.
AAPT="$(ls -d "$ANDROID_HOME"/build-tools/*/aapt2 2>/dev/null | sort -V | tail -1)"
if [ -n "$AAPT" ]; then
  "$AAPT" dump badging "$APK" | grep -E "^package:|^launchable-activity:"
fi
ls -lh "$APK"

mkdir -p build && cp "$APK" build/
