#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# This script has not been run on any machine. It was written from
# RUNNER_SPEC §18 as a starting point for the M0 manual baseline, not as a
# record of anything that worked. Expect it to be wrong in at least one place.
# When you run M0 by hand (docs/manual-baseline.md), replace this file with the
# commands that actually succeeded and delete this banner.
#
# Requires in the environment:
#   (none — this step declares no secrets)
#
# Optional, set by the agent for every step:
#   LATEM_ENV   path to the run's env file; append KEY=value to export a value
#               to LATER steps (§9). Nothing else survives a step boundary.
# ---------------------------------------------------------------------------

# Each step is a NEW PROCESS (§9). Exporting PATH here does not reach the next
# step; installing a binary here does, because the filesystem persists. If a
# later step needs a value computed here, it goes through $LATEM_ENV and
# nowhere else.

echo "--- environment"
node --version
uname -sm

# Corepack pins the pnpm version from package.json#packageManager, so the
# cached store and the lockfile cannot drift apart between the Mac and the
# container.
corepack enable
corepack prepare --activate

# The pnpm store is a mounted cache (§14): mounted, not restored, so there is
# no save step and nothing to upload. Point pnpm at the mount path declared in
# .latem/jobs.yaml — pnpm otherwise defaults to a path inside $HOME that is not
# the mount and the cache silently does nothing.
if [[ "$(uname -s)" == "Darwin" ]]; then
  PNPM_STORE="$HOME/.pnpm-store"
else
  PNPM_STORE="/home/runner/.pnpm-store"
fi
pnpm config set store-dir "$PNPM_STORE"

# --frozen-lockfile: a CI install that silently updates the lockfile has built
# something other than what the commit says.
pnpm install --frozen-lockfile

# CocoaPods only exists on the macOS path. `npx expo prebuild` generates the
# ios/ directory for a managed Expo project; if the repo is bare-workflow the
# directory is committed and this is a no-op check.
if [[ "$(uname -s)" == "Darwin" && -d ios ]]; then
  echo "--- pod install"
  # UNVERIFIED: `eas build --local` may run pod install itself, in which case
  # this is redundant work on every iOS run. Confirm during M0 and delete this
  # block if so — it is several minutes per build.
  ( cd ios && bundle exec pod install )
fi

# An example of the only sanctioned cross-step channel. Keys must match
# ^[A-Za-z_][A-Za-z0-9_]*$; keys colliding with a declared secret or with
# LATEM_* are ignored by the agent rather than honoured.
echo "BUILD_NUMBER=$(git rev-list --count HEAD)" >> "${LATEM_ENV:-/dev/null}"
