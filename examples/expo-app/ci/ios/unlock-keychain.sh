#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# Written from RUNNER_SPEC §18.1 as M0 scaffolding. No machine has run it, and
# the diagnostics below are reasoned from the spec rather than observed. The
# `security` invocations come from §18.1 verbatim; everything around them is a
# guess at what makes the failure legible. Replace with what worked.
#
# Requires in the environment:
#   KEYCHAIN_PASSWORD   login keychain password for the auto-login user
#
# Requires of the machine (docs/ios.md):
#   - a real GUI user session — a LaunchAgent, not a LaunchDaemon (§13.3, §23)
#   - full Xcode selected, not the Command Line Tools
# ---------------------------------------------------------------------------

KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"

# Fail loudly and early on the two conditions that otherwise surface 40 minutes
# later as `User interaction is not allowed` from codesign — an error that names
# nothing useful and sends people to Stack Overflow instead of to the cause.

# 1. Session context. `launchctl managername` prints "Aqua" in a GUI session,
#    "Background" or "System" in a daemon context. codesign needs the former:
#    the keychain of a user with no session cannot be unlocked for signing no
#    matter how correct the password is.
#    UNVERIFIED: confirm the exact string on macOS 26 — this check is worth
#    having only if it is accurate, and a wrong check is worse than none.
if command -v launchctl >/dev/null; then
  session="$(launchctl managername 2>/dev/null || echo unknown)"
  echo "launchd session: $session"
  if [[ "$session" != "Aqua" ]]; then
    echo "ERROR: not running in a GUI session (managername=$session)." >&2
    echo "  codesign cannot reach the login keychain from a daemon context." >&2
    echo "  Install the agent as a LaunchAgent under an auto-login user." >&2
    echo "  See docs/ios.md, 'LaunchAgent vs LaunchDaemon'." >&2
    exit 1
  fi
fi

# 2. Xcode selection. The CLT package has no iOS SDK, so an `xcode-select`
#    pointing at it fails later with `SDK "iphoneos" cannot be located`.
developer_dir="$(xcode-select -p)"
echo "xcode-select -p: $developer_dir"
if [[ "$developer_dir" == *"CommandLineTools"* ]]; then
  echo "ERROR: Command Line Tools are selected instead of full Xcode." >&2
  echo "  Fix: sudo xcode-select -s /Applications/Xcode.app/Contents/Developer" >&2
  exit 1
fi

if [[ ! -f "$KEYCHAIN" ]]; then
  echo "ERROR: no login keychain at $KEYCHAIN" >&2
  echo "  The agent is probably running as a user that has never logged in." >&2
  exit 1
fi

# The unlock itself (§18.1). -p reads the password from argv, which is visible
# in `ps` for the instant the process lives; the alternative is a prompt, and a
# prompt on an unattended machine is a hang. The agent's redactor covers the
# value in logs, not in the process table — a shared machine is out of scope.
security unlock-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN"

# -t 3600  lock again after an hour of inactivity
# -u       lock on sleep
#
# Deliberately not "never lock": a build takes well under an hour, and a
# keychain left permanently unlocked on a machine holding distribution
# certificates is a standing risk for no gain. If a build legitimately exceeds
# this, raise the number rather than removing the timeout.
security set-keychain-settings -t 3600 -u "$KEYCHAIN"

# Put the login keychain first in the search list. `match` imports into it, and
# a search list that omits it produces "no signing certificate found" while the
# certificate is demonstrably installed — a failure mode that reads as a
# fastlane bug and is not one.
security list-keychains -d user -s "$KEYCHAIN"

echo "keychain unlocked: $KEYCHAIN"
