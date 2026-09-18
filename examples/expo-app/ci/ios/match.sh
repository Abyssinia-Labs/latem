#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# Written from RUNNER_SPEC §18.1 as M0 scaffolding. No machine has run it. The
# `fastlane match` line is from §18.1 verbatim; the rest is inference. Replace
# with what worked during the manual baseline.
#
# Requires in the environment:
#   MATCH_PASSWORD   passphrase decrypting the certs repo
#   MATCH_GIT_URL    git URL of the private certificates repository
#
# Requires of the machine:
#   - the login keychain unlocked (ci/ios/unlock-keychain.sh ran first)
#   - an SSH key or credential that can READ the certs repo
# ---------------------------------------------------------------------------

# Signing material lives encrypted in a repository the operator controls. The
# agent supplies MATCH_PASSWORD and nothing else about signing touches it; no
# certificate, key, or profile ever crosses the broker boundary (INV-4). This is
# the whole reason match is used instead of certificates baked into an image.

: "${MATCH_PASSWORD:?not set — declare it in .latem/jobs.yaml secrets:}"
: "${MATCH_GIT_URL:?not set — declare it in .latem/jobs.yaml secrets:}"

# --readonly is not optional here, and it is the single most important flag in
# this file. Without it, match will happily CREATE a new distribution
# certificate when it cannot find one — silently consuming one of the account's
# limited certificate slots and, worse, revoking the certificate every other
# machine and developer is using. A CI runner must only ever consume signing
# material. Creating it is a human action, run once, from a laptop.
bundle exec fastlane match appstore --readonly

# Prove the certificate actually landed in the keychain rather than trusting
# match's exit code. A green match followed by a signing failure 40 minutes into
# the build is the expensive version of this check.
echo "--- identities available for signing"
security find-identity -v -p codesigning

# Captured and matched in the shell, no pipeline (amendment A37). Piped into
# `grep -q`, this check reports "no identity" on a keychain that has one —
# intermittently, and only on a loaded machine, which is what a runner is.
identities=$(security find-identity -v -p codesigning)
if [[ $identities != *"Apple Distribution"* ]]; then
  echo "ERROR: no Apple Distribution identity in the keychain after match." >&2
  echo "  Likely causes, in order:" >&2
  echo "   - wrong MATCH_GIT_URL (pointing at an empty or wrong certs repo)" >&2
  echo "   - keychain not unlocked, so the import silently no-op'd" >&2
  echo "   - the certs repo has no appstore profile for this bundle id yet;" >&2
  echo "     run 'fastlane match appstore' WITHOUT --readonly once, by hand," >&2
  echo "     from a developer machine — never from the runner." >&2
  exit 1
fi
