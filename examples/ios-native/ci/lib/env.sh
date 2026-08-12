# Shared step preamble. Sourced by every script in ci/, never executed.
#
# WHY THIS FILE EXISTS
#
# A step is a new process with only the environment the agent hands it (§9), and
# on the native executor (§13.3, the default since A31) that environment is
# whatever the LaunchAgent inherited — not the operator's interactive shell. So
# anything a developer set up by typing `export` into a terminal is absent, and
# the failures that produces name neither the environment nor the export.
#
# Two have been observed, both on 2026-08-11, both after a successful build:
#
#   1. `/usr/bin/bundle` is macOS's system Ruby 2.6. With Homebrew's keg-only
#      Ruby not on PATH it wins, and reports:
#        Could not find 'bundler' (4.0.18) required by your Gemfile.lock
#      which reads as a bundler version problem and is a wrong-Ruby problem.
#      `gem install bundler:4.0.18` "fixes" it by installing into 2.6, where
#      fastlane then refuses to run.
#
#   2. CocoaPods aborts on a non-UTF-8 locale with
#        Unicode Normalization not appropriate for ASCII-8BIT
#      which names Unicode rather than LANG.
#
# Both are invisible on the machine that was set up by hand and certain on a
# fresh one. Sourcing this file is what makes a step reproducible rather than
# dependent on who last typed into a terminal on that Mac.

# Homebrew Ruby first. Keg-only, so it is never on PATH by default — that is the
# whole trap. Both Apple-silicon and Intel prefixes are checked so this is not
# silently a no-op on an Intel mini.
for _rb in /opt/homebrew/opt/ruby/bin /usr/local/opt/ruby/bin; do
  [ -d "$_rb" ] && PATH="$_rb:$PATH"
done
unset _rb
export PATH

# Gems installed by ci/install.sh into vendor/bundle put their binstubs here.
[ -d "$PWD/vendor/bundle/bin" ] && export PATH="$PWD/vendor/bundle/bin:$PATH"

export LANG="${LANG:-en_US.UTF-8}"
export LC_ALL="${LC_ALL:-en_US.UTF-8}"

# Fail here, naming the cause, rather than 40 minutes later inside fastlane.
# The version floor is the Gemfile's: modern fastlane will not install against
# macOS's 2.6, and the error when it tries is a wall of native-extension
# compile output that mentions no version number at all.
_ruby_ok=$(ruby -e 'print Gem::Version.new(RUBY_VERSION) >= Gem::Version.new("3.1") ? "yes" : "no"' 2>/dev/null || echo no)
if [ "$_ruby_ok" != "yes" ]; then
  echo "ERROR: ruby $(ruby -e 'print RUBY_VERSION' 2>/dev/null || echo '(not found)') at $(command -v ruby || echo '?') is too old; need >= 3.1" >&2
  echo "       macOS ships 2.6 at /usr/bin/ruby and Homebrew's is keg-only." >&2
  echo "       fix: brew install ruby   (this script adds it to PATH itself)" >&2
  exit 1
fi
unset _ruby_ok
