#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# Written from RUNNER_SPEC §18 as M0 scaffolding. No machine has run it.
# Replace with what actually worked during the manual baseline.
#
# Requires in the environment:
#   (none — this step declares no secrets)
# ---------------------------------------------------------------------------

# This step is a new process, so nothing ci/install.sh exported is present.
# Anything it installed into the filesystem is.

# Sequenced here rather than as three separate steps in jobs.yaml on purpose:
# these share nothing but a working directory, and three steps would mean three
# `docker exec` round trips for no benefit. Steps exist to give a timeout and a
# name to a unit of work worth timing separately, not to enumerate commands.

echo "--- typecheck"
pnpm exec tsc --noEmit

echo "--- lint"
pnpm exec eslint . --max-warnings=0

echo "--- unit tests"
# --ci makes Jest non-interactive and fails rather than writing new snapshots.
# Without it a missing snapshot is silently created and the test passes, which
# is the CI equivalent of not running the test at all.
pnpm exec jest --ci --runInBand
