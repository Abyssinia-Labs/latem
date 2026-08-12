#!/usr/bin/env bash
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"

# ---------------------------------------------------------------------------
# Create the Play upload keystore. Run ONCE, by a human, on a trusted machine.
#
# This is the Android analogue of `fastlane match` — except there is no `match`
# for Android, so the keystore is a file you must not lose and must not commit.
#
# Why a script rather than a command in a README: `keytool` on a Mac resolves to
# /usr/bin/keytool, which is Apple's stub, and it fails with
#   The operation couldn't be completed. Unable to locate a Java Runtime.
# even when a perfectly good JDK 17 is installed — because Homebrew's JDK is
# keg-only and never on PATH. Sourcing ci/lib/env.sh finds it. That trap has
# now caught /usr/bin/java, /usr/bin/bundle and /usr/bin/keytool in one session.
#
#   ./ci/android/make-upload-keystore.sh [path]
#
# Defaults to ~/latem-upload.keystore. You will be asked for a password twice.
# ---------------------------------------------------------------------------

KEYSTORE="${1:-$HOME/latem-upload.keystore}"
ALIAS="${ANDROID_KEY_ALIAS:-upload}"

if [ -e "$KEYSTORE" ]; then
  echo "ERROR: $KEYSTORE already exists." >&2
  echo "       Refusing to overwrite: if this is the key your app ships with," >&2
  echo "       replacing it means you can no longer publish updates without" >&2
  echo "       Google's key reset process. Move it aside deliberately if you" >&2
  echo "       really want a new one." >&2
  exit 1
fi

# PKCS12 is the standard format; without -storetype, keytool creates a
# proprietary JKS and then warns about it on every single use.
#
# -dname is supplied so this does not stop for six identity prompts. None of the
# fields are verified by Google or shown to users — an upload key proves
# continuity, not identity. Override by exporting KEYSTORE_DNAME.
"$JAVA_HOME/bin/keytool" -genkeypair -v \
  -keystore "$KEYSTORE" \
  -storetype PKCS12 \
  -alias "$ALIAS" \
  -keyalg RSA -keysize 2048 \
  -validity 10000 \
  -dname "${KEYSTORE_DNAME:-CN=Latem Upload Key, O=Abyssinia Labs, C=US}"

chmod 600 "$KEYSTORE"

echo
echo "Created $KEYSTORE (alias: $ALIAS)"
echo
echo "  ANDROID_KEY_ALIAS=$ALIAS"
echo "  ANDROID_KEYSTORE_PASSWORD  = the password you just chose"
echo "  ANDROID_KEY_PASSWORD       = the same password (PKCS12 uses one)"
echo
echo "For the runner, the keystore travels base64-encoded as one line:"
echo
echo "  export ANDROID_KEYSTORE_B64=\"\$(base64 -i '$KEYSTORE')\""
echo
echo "Back it up somewhere you will still have in three years. Losing this file"
echo "or its password means you cannot ship an update to this app under this"
echo "key, and recovering needs Google's key reset process."
