#!/usr/bin/env bash
set -euo pipefail

# Rewrites SHA256SUMS for the versions the Dockerfile's ARGs name.
#
# Everything the image downloads outside apt is checked against that file, so
# bumping a version is: edit the ARG, run this, read the diff, rebuild. Node
# publishes checksums and GitHub reports a digest for each release asset, so
# neither costs a download here. AWS publishes only a PGP signature, so its two
# zips (~60 MB each) are fetched and hashed.
#
# Requires curl and jq.

here=$(cd "$(dirname "$0")" && pwd)
dockerfile=$here/Dockerfile

for tool in curl jq; do
  command -v "$tool" >/dev/null || { echo "pin.sh: $tool is required" >&2; exit 1; }
done

if command -v sha256sum >/dev/null; then
  sha256() { sha256sum "$1"; }
else
  sha256() { shasum -a 256 "$1"; }
fi

# arg NAME prints the default the Dockerfile gives `ARG NAME=…`.
arg() {
  local value
  value=$(sed -n "s/^ARG $1=//p" "$dockerfile")
  if [[ -z $value || $value == *$'\n'* ]]; then
    echo "pin.sh: expected exactly one 'ARG $1=…' in $dockerfile" >&2
    exit 1
  fi
  printf '%s\n' "$value"
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# hash_url URL NAME downloads URL and prints a SHA256SUMS line for NAME.
hash_url() {
  local digest
  curl -fsSL --retry 3 -o "$work/$2" "$1"
  digest=$(sha256 "$work/$2")
  rm "$work/$2"
  printf '%s  %s\n' "${digest%% *}" "$2"
}

lines=()

for v in "$(arg NODE22_VERSION)" "$(arg NODE24_VERSION)"; do
  sums=$(curl -fsSL --retry 3 "https://nodejs.org/dist/v$v/SHASUMS256.txt")
  for a in x64 arm64; do
    line=$(awk -v f="node-v$v-linux-$a.tar.xz" '$2 == f' <<<"$sums")
    [[ -n $line ]] || { echo "pin.sh: node-v$v-linux-$a.tar.xz is not in Node's SHASUMS256.txt" >&2; exit 1; }
    lines+=("$line")
  done
done

for rel in "$(arg PYTHON311_RELEASE)" "$(arg PYTHON313_RELEASE)"; do
  v=${rel%-*}
  release=$(curl -fsSL --retry 3 "https://api.github.com/repos/actions/python-versions/releases/tags/$rel")
  for a in x64 arm64; do
    f=python-$v-linux-24.04-$a.tar.gz
    digest=$(jq -r --arg f "$f" '.assets[] | select(.name == $f) | .digest // ""' <<<"$release")
    if [[ $digest == sha256:* ]]; then
      lines+=("${digest#sha256:}  $f")
    elif [[ -z $digest ]] && ! jq -e --arg f "$f" 'any(.assets[]; .name == $f)' <<<"$release" >/dev/null; then
      echo "pin.sh: release $rel has no asset $f" >&2
      exit 1
    else
      # Assets uploaded before GitHub began recording digests have none.
      lines+=("$(hash_url "https://github.com/actions/python-versions/releases/download/$rel/$f" "$f")")
    fi
  done
done

aws=$(arg AWSCLI_VERSION)
for a in x86_64 aarch64; do
  f=awscli-exe-linux-$a-$aws.zip
  lines+=("$(hash_url "https://awscli.amazonaws.com/$f" "$f")")
done

printf '%s\n' "${lines[@]}" | sort -k2 >"$here/SHA256SUMS.new"
mv "$here/SHA256SUMS.new" "$here/SHA256SUMS"
echo "wrote $here/SHA256SUMS (${#lines[@]} files)"
