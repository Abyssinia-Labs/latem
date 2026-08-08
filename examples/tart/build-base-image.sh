#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# UNVERIFIED — DRAFT SCAFFOLDING, NEVER EXECUTED
#
# Builds the `latem-xcode-26` Tart base image described in RUNNER_SPEC §13.2.
#
# Tart is NOT installed on the machine this script was written on. No line here
# has been executed, no image has been built, and the timings and sizes below
# are estimates from the spec rather than measurements. Treat this as an
# annotated checklist you drive by hand the first time, not as automation you
# trust. When it works, record the real numbers in docs/manual-baseline.md and
# delete this banner.
#
# Requires on the HOST:
#   tart          brew install cirruslabs/cli/tart
#   An Apple ID with a developer account, for the Xcode download.
#   ~200 GB free. The finished image is ~90 GB (§13.2) and the intermediate
#   Xcode .xip plus its expansion transiently needs roughly as much again.
#
# Requires in the environment:
#   XCODE_VERSION       e.g. "26.0.1" — pin it; "latest" makes the image
#                       unreproducible and silently changes the toolchain
#   VM_NAME             defaults to latem-xcode-26
#   BASE_IMAGE          upstream macOS image to clone from
#   SSH_PASS            password for the guest `admin` user (Cirrus images
#                       ship with a known default; change it or accept it)
#
# Time: expect 2–4 hours, most of it the Xcode download and .xip expansion.
# Run it somewhere you can leave it.
# ---------------------------------------------------------------------------

VM_NAME="${VM_NAME:-latem-xcode-26}"
XCODE_VERSION="${XCODE_VERSION:?pin an exact Xcode version, e.g. 26.0.1}"
BASE_IMAGE="${BASE_IMAGE:-ghcr.io/cirruslabs/macos-sequoia-base:latest}"
SSH_USER="${SSH_USER:-admin}"
SSH_PASS="${SSH_PASS:-admin}"
DISK_SIZE_GB="${DISK_SIZE_GB:-200}"

command -v tart >/dev/null || {
  echo "tart is not installed: brew install cirruslabs/cli/tart" >&2
  exit 1
}

# Why an operator task, not a build step: Apple's licence permits two guest VMs
# per host (§13.2), Xcode requires an authenticated download, and the image is
# ~90 GB. This is rebuilt on Xcode upgrades, by a person, deliberately — a
# nightly job that rebuilds it would saturate the disk and the licence at once.

if tart list --format json | grep -q "\"$VM_NAME\""; then
  echo "VM '$VM_NAME' already exists."
  echo "Delete it first if you mean to rebuild: tart delete $VM_NAME"
  exit 1
fi

echo "=== 1/7  clone the upstream base image"
# This pulls tens of gigabytes on first use. Subsequent clones of a LOCAL image
# are APFS copy-on-write and near-instant; this one is a network pull.
tart clone "$BASE_IMAGE" "$VM_NAME"

echo "=== 2/7  size the disk"
# The default is too small to hold Xcode plus a DerivedData tree. Growing a Tart
# disk after the fact is possible but fiddly; size it once, here.
tart set "$VM_NAME" --disk-size "$DISK_SIZE_GB"
tart set "$VM_NAME" --cpu 6 --memory 16384

echo "=== 3/7  boot"
tart run --no-graphics "$VM_NAME" &
TART_PID=$!
trap 'kill "$TART_PID" 2>/dev/null || true' EXIT

echo "waiting for an IP..."
for _ in $(seq 1 60); do
  if IP="$(tart ip "$VM_NAME" 2>/dev/null)" && [[ -n "$IP" ]]; then break; fi
  sleep 5
done
[[ -n "${IP:-}" ]] || { echo "VM never got an IP" >&2; exit 1; }
echo "guest IP: $IP"

# sshpass is the pragmatic choice for a one-off provisioning script against a
# known-credential image. If you would rather not install it, run the blocks
# below by hand over a normal ssh session — they are written to be pasteable.
ssh_run() {
  sshpass -p "$SSH_PASS" ssh \
    -o StrictHostKeyChecking=accept-new \
    -o UserKnownHostsFile=/dev/null \
    "$SSH_USER@$IP" "bash -lc '$1'"
}

echo "=== 4/7  Homebrew, Node, and the Ruby toolchain"
# System Ruby on macOS is 2.6 and modern fastlane will not install against it.
# This is the single most common wasted hour when setting a Mac up for iOS CI,
# so the image bakes in a Homebrew Ruby and puts it ahead of /usr/bin on PATH.
ssh_run '/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)" </dev/null'
ssh_run 'echo "eval \"\$(/opt/homebrew/bin/brew shellenv)\"" >> ~/.zprofile'
ssh_run 'brew install ruby node@22 cocoapods xcodes aria2'
# Ruby first on PATH, permanently, for every login shell. The Tart executor runs
# each step as `bash -lc`, so a login shell is what jobs get (§13.2).
ssh_run 'echo "export PATH=/opt/homebrew/opt/ruby/bin:/opt/homebrew/lib/ruby/gems/3.3.0/bin:\$PATH" >> ~/.zprofile'
ssh_run 'echo "export PATH=/opt/homebrew/opt/node@22/bin:\$PATH" >> ~/.zprofile'
ssh_run 'ruby --version && node --version'

echo "=== 5/7  fastlane and bundler"
ssh_run 'gem install bundler fastlane --no-document'
ssh_run 'fastlane --version'

echo "=== 6/7  Xcode ${XCODE_VERSION}"
# INTERACTIVE. `xcodes install` prompts for an Apple ID and, usually, a 2FA
# code. There is no unattended path that does not involve storing an Apple ID
# password in the image, which is worse than typing a code once.
#
# Budget 60–120 minutes: the download is ~10 GB and the .xip expansion is
# single-threaded and slow.
echo "--- this step will prompt for an Apple ID and a 2FA code ---"
sshpass -p "$SSH_PASS" ssh -t \
  -o StrictHostKeyChecking=accept-new \
  -o UserKnownHostsFile=/dev/null \
  "$SSH_USER@$IP" "bash -lc 'xcodes install $XCODE_VERSION --experimental-unxip'"

ssh_run "sudo xcode-select -s /Applications/Xcode-${XCODE_VERSION}.app/Contents/Developer"
# Accepting the licence and installing the bundled packages here means no build
# ever stops on a licence prompt — which in a headless VM is an invisible hang,
# not an error.
ssh_run 'sudo xcodebuild -license accept'
ssh_run 'sudo xcodebuild -runFirstLaunch'
ssh_run 'xcodebuild -version && xcrun --sdk iphoneos --show-sdk-path'

# Pre-download the simulator runtime. Otherwise the first job to need it pays a
# multi-gigabyte download inside its own timeout.
ssh_run 'xcodebuild -downloadPlatform iOS' || \
  echo "WARN: simulator runtime download failed; first sim job will be slow"

echo "=== 7/7  shut down cleanly and report size"
ssh_run 'sudo shutdown -h now' || true
sleep 30
tart stop "$VM_NAME" 2>/dev/null || true

echo
echo "=== image built: $VM_NAME"
du -sh "$HOME/.tart/vms/$VM_NAME" || true
cat <<'NOTES'

Expected size: ~90 GB (RUNNER_SPEC §13.2). If yours is materially different,
that number is a placeholder in the spec and yours is the real one — record it
in docs/manual-baseline.md, because `min_free_gb = 150` for the tart runner is
derived from it.

  Base image           ~90 GB
  Per-run divergence   unmeasured; a running VM diverges from the CoW clone and
                       DerivedData alone is tens of GB. Measure in M0.

Operator tasks this creates, permanently:

  - REBUILD ON XCODE UPGRADE. The image name pins the major version
    (latem-xcode-26). When Xcode 27 ships, build latem-xcode-27 alongside it,
    repoint `base_image` in config.toml, and delete the old image only once a
    real build has passed on the new one. Never upgrade Xcode in place inside
    the image: the point of the image is that it is reproducible.
  - Apple's licence permits TWO guest VMs per host. `concurrency` for a tart
    runner must be <= 2, and the config loader enforces it (§8, §13.2).
  - `tart list` after a crash will show leaked latem-run-* clones. Each one
    costs ~90 GB nominally and is the fastest way to fill a Mac mini (§14.4).

NOTES
