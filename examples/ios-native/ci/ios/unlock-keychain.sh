#!/usr/bin/env bash
set -euo pipefail

# Every ci/ script sources this: it puts Homebrew Ruby on PATH and sets the
# locale, neither of which a runner step inherits. See ci/lib/env.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"
# UNVERIFIED — never run. §18.1 verbatim.
#
# On the native executor (§13.3) this unlocks the OPERATOR'S OWN login keychain,
# because a step runs as the logged-in user. That is the isolation trade §13.0
# describes, stated concretely: this script has the same access to that keychain
# as the person sitting at the machine.
: "${KEYCHAIN_PASSWORD:?not set}"
security unlock-keychain -p "$KEYCHAIN_PASSWORD" "$HOME/Library/Keychains/login.keychain-db"
security set-keychain-settings -t 3600 -u "$HOME/Library/Keychains/login.keychain-db"
