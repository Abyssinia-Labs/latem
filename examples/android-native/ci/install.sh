#!/usr/bin/env bash
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/env.sh"

# Gradle itself is NOT installed by this script and must not be. ./gradlew
# bootstraps the exact version pinned in gradle/wrapper/gradle-wrapper.properties,
# which is committed — so the runner builds with the version this repo says,
# not whatever brew last put on the machine.
bundle config set --local path vendor/bundle
bundle install --quiet
bundle exec fastlane --version >/dev/null

# pipefail-safe: the version banner, same reasoning as the iOS install script.
java_version=$( ("$JAVA_HOME/bin/java" -version 2>&1 || true) | sed -n '1s/.*"\(.*\)".*/\1/p')
echo "java ${java_version:-?}  sdk $ANDROID_HOME"
