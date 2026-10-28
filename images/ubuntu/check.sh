#!/usr/bin/env bash
set -euo pipefail

# Checks a built latem-ubuntu image and prints its size.
#
#   ./check.sh [IMAGE]       IMAGE defaults to latem-ubuntu:local
#
# Everything runs as 1000:1000, which is what latem gives a step on a Linux
# host whose agent is uid 1000 (§13.1). Three groups:
#
#   contract   §13.1. The same facts `latem doctor` collects with its probe
#              (imageProbeScript in internal/executor/docker/image.go), plus
#              item 3: nothing under $HOME belongs to another uid. doctor is
#              the authority; this exists so an image can be checked before
#              any runner's config names it.
#   tools      what README.md says a job can call without installing it.
#   behaviour  sudo, apt, `npm install -g`, a venv, and Playwright's own check
#              that Chromium's system packages are present. Needs the network.

here=$(cd "$(dirname "$0")" && pwd)
image=${1:-latem-ubuntu:local}
playwright=$(sed -n 's/^ARG PLAYWRIGHT_VERSION=//p' "$here/Dockerfile")

# Runs inside the image under bash. No `set -e`: every check runs and reports,
# and the exit status says whether any failed.
script=$(
  cat <<'EOF'
fails=0

# check NAME COMMAND… runs COMMAND and reports the first line it printed.
check() {
  local name=$1 out
  shift
  if out=$("$@" 2>&1); then
    printf 'ok    %-14s %s\n' "$name" "${out%%$'\n'*}"
  else
    printf 'FAIL  %-14s %s\n' "$name" "${out%%$'\n'*}"
    fails=$((fails + 1))
  fi
}

user_1000() {
  echo "uid $(id -u) is $(id -un), gid $(id -g) is $(id -gn)"
  [[ $(id -un) == runner && $(id -gn) == runner ]]
}
home_writable() {
  echo "HOME=${HOME:-unset}"
  [[ -n ${HOME:-} && -d $HOME ]] && touch "$HOME/.latem-check" && rm "$HOME/.latem-check"
}
# owned_by_1000 DIR fails, naming them, if anything under DIR has another owner.
owned_by_1000() {
  local stray
  stray=$(find "$1" ! -uid 1000 -print)
  if [[ -n $stray ]]; then
    echo "not uid 1000: ${stray//$'\n'/ }"
    return 1
  fi
  echo "everything under $1 is uid 1000"
}
ca_bundle() { ls /etc/ssl/certs/ca-certificates.crt; }
node22() { "$RUNNER_TOOL_CACHE"/node/22.*/*/bin/node --version; }
no_bun() {
  if command -v bun; then return 1; fi
  echo "not baked in; setup-bun installs it per job"
}
sudo_ok() { sudo -n true && echo "passwordless"; }
apt_ok() { sudo -n apt-get update -qq && echo "apt-get update succeeded"; }
chrome_list() { sudo -n rm -f /etc/apt/sources.list.d/google-chrome* && echo "sudo rm succeeded"; }
npm_global() {
  npm install -g npm@11 --no-audit --no-fund --loglevel=error >/dev/null || return
  local v
  v=$(npm --version)
  echo "npm $v in $(npm prefix -g)"
  [[ $v == 11.* ]]
}
venv_ok() { python3.13 -m venv /tmp/venv && /tmp/venv/bin/python -m pip --version; }
playwright_deps() { npx --yes "playwright@$PLAYWRIGHT" install-deps --dry-run chromium; }

echo "-- contract"
check user-1000      user_1000
check home-writable  home_writable
check home-owned     owned_by_1000 "$HOME"
check tool-bash      bash --version
check tool-git       git --version
check ca-bundle      ca_bundle

echo "-- tools"
check git-lfs        git lfs version
check curl           curl --version
check jq             jq --version
check gh             gh --version
check aws            aws --version
check zip            command -v zip
check unzip          command -v unzip
check tar            tar --version
check sha256sum      sha256sum --version
check gcc            gcc --version
check make           make --version
check python3        python3 --version
check python3.11     python3.11 --version
check python3.13     python3.13 --version
check node           node --version
check node22         node22
check npm            npm --version
check docker         docker --version
check buildx         docker buildx version
check compose        docker compose version
check no-bun         no_bun

echo "-- behaviour"
check sudo           sudo_ok
check toolcache      owned_by_1000 "$RUNNER_TOOL_CACHE"
# Before apt-get update on purpose: with package lists present, a library
# Ubuntu has updated since the build would be reported as missing.
check playwright     playwright_deps
check apt-get        apt_ok
check chrome-list    chrome_list
check npm-global     npm_global
check venv           venv_ok

echo "-- $fails failed"
[[ $fails -eq 0 ]]
EOF
)

status=0
docker run --rm --user 1000:1000 --env PLAYWRIGHT="$playwright" "$image" /bin/bash -c "$script" || status=$?

bytes=$(docker image inspect --format '{{.Size}}' "$image")
arch=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
echo "-- size: $((bytes / 1000000)) MB uncompressed ($arch)"
exit "$status"
