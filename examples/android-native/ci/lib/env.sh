# Shared step preamble. Sourced by every script in ci/, never executed.
#
# Same purpose as examples/ios-native/ci/lib/env.sh, and it exists for the same
# reason: a step is a new process with only the environment the agent hands it
# (§9), and on the native executor (§13.3) that is the LaunchAgent's environment,
# not the operator's shell. Android adds two of its own traps.

# 1. JAVA_HOME must be JDK 17, and must be SET.
#
# `brew install gradle` pulls in the CURRENT openjdk as a dependency — 26.0.2 on
# this machine — and Gradle then picks it as the daemon JVM. AGP does not accept
# it. Meanwhile /usr/bin/java on a Mac with no JDK is a stub that prints "Unable
# to locate a Java Runtime", so `java -version` succeeding proves nothing about
# which JDK a build will use.
#
# openjdk@17 is keg-only, exactly like Homebrew's ruby, so it is never on PATH
# by default. That is the trap, twice, in two languages.
for _jh in /opt/homebrew/opt/openjdk@17 /usr/local/opt/openjdk@17 \
           /Library/Java/JavaVirtualMachines/temurin-17.jdk/Contents/Home; do
  if [ -x "$_jh/bin/javac" ]; then
    export JAVA_HOME="$_jh"
    PATH="$_jh/bin:$PATH"
    break
  fi
done
unset _jh
export PATH

if [ -z "${JAVA_HOME:-}" ]; then
  echo "ERROR: no JDK 17 found." >&2
  echo "       AGP 9 requires 17; a newer JDK (Gradle may pull in 26) is rejected." >&2
  echo "       fix: brew install openjdk@17   (this script finds it; it is keg-only)" >&2
  exit 1
fi

# 2. ANDROID_HOME. Gradle fails with a message about a missing SDK location and
# suggests writing local.properties — which is per-machine and must not be
# committed, so a runner would need it generated per run. Setting the variable
# here is the reproducible form.
for _sdk in "${ANDROID_HOME:-}" "${ANDROID_SDK_ROOT:-}" "$HOME/Library/Android/sdk" \
            /opt/homebrew/share/android-commandlinetools /usr/local/share/android-sdk; do
  if [ -n "$_sdk" ] && [ -d "$_sdk/platform-tools" ]; then
    export ANDROID_HOME="$_sdk"
    export ANDROID_SDK_ROOT="$_sdk"
    PATH="$_sdk/platform-tools:$PATH"
    break
  fi
done
unset _sdk
export PATH

if [ -z "${ANDROID_HOME:-}" ]; then
  echo "ERROR: no Android SDK found (looked for a platform-tools/ inside each candidate)." >&2
  echo "       fix: install via Android Studio, or brew install android-commandlinetools" >&2
  exit 1
fi

# 3. Homebrew Ruby, for fastlane's `supply`. Identical to the iOS example's
# env.sh and omitted from the first draft of this file, which is the third time
# this exact trap has been sprung in one session: macOS ships Ruby 2.6, Homebrew
# ruby is keg-only, and `./ci/install.sh` fails with
#   Your Ruby version is 2.6.10, but your Gemfile specified >= 3.1
# The Android build itself does not need Ruby at all — only the Play upload
# does — so this failure appears one step later than you expect.
for _rb in /opt/homebrew/opt/ruby/bin /usr/local/opt/ruby/bin; do
  [ -d "$_rb" ] && PATH="$_rb:$PATH"
done
unset _rb
export PATH
[ -d "$PWD/vendor/bundle/bin" ] && export PATH="$PWD/vendor/bundle/bin:$PATH"

export LANG="${LANG:-en_US.UTF-8}"
export LC_ALL="${LC_ALL:-en_US.UTF-8}"
