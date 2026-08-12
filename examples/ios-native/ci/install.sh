#!/usr/bin/env bash
set -euo pipefail

# Every ci/ script sources this: it puts Homebrew Ruby on PATH and sets the
# locale, neither of which a runner step inherits. See ci/lib/env.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/./lib/env.sh"

# There is no JavaScript dependency graph to install — no node_modules, no Pods.
# That is the difference between this example and examples/expo-app, and it is
# why this one builds on a machine where that one does not (docs/ios.md).

# Install gems into the workspace rather than a shared user gem directory.
#
# This is not tidiness. Verified on 2026-08-11: `bundle install` reported
# "Bundle complete! 97 gems now installed" and `bundle exec fastlane` then said
# `bundler: command not found: fastlane`. The gem was genuinely installed and
# `require "fastlane"` worked — but its binstub landed in ~/.gem/ruby/3.4.0/bin
# while bundler's EXECUTABLE DIRECTORY was /opt/homebrew/lib/ruby/gems/3.4.0/bin,
# so the library resolved and the executable did not. Two Rubies and a keg-only
# Homebrew install is enough to produce that split, and the error message
# suggests exactly the command that had just been run and did not help.
#
# A workspace-local path removes the ambiguity: there is one gem directory, the
# agent's fresh clone (§12) starts without it, and nothing on the host leaks in.
# It matters more on the native executor (§13.3, the default since A31), where
# no VM resets the machine and a stale user gem dir survives indefinitely.
#
# --local writes .bundle/config, which is gitignored, so this line rather than
# the file is what makes a runner reproduce it.
bundle config set --local path vendor/bundle
bundle install --quiet

bundle exec fastlane --version >/dev/null
echo "ruby $(ruby -e 'print RUBY_VERSION')  xcode $(xcodebuild -version | head -1 | cut -d' ' -f2)"
