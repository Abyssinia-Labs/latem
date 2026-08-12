#!/usr/bin/env bash
set -euo pipefail

# Every ci/ script sources this: it puts Homebrew Ruby on PATH and sets the
# locale, neither of which a runner step inherits. See ci/lib/env.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"
# UNVERIFIED — never run. Needs MATCH_GIT_URL to point at a private certs repo
# that does not exist yet (manual-baseline.md §2).
: "${MATCH_PASSWORD:?not set}"
: "${MATCH_GIT_URL:?not set}"
# --readonly is load-bearing: without it match will CREATE certificates, and an
# account has a hard cap on how many distribution certs can exist. A runner that
# creates one per build exhausts it and then breaks every developer's Xcode.
bundle exec fastlane match appstore --readonly
