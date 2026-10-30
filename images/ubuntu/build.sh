#!/usr/bin/env bash
set -euo pipefail

# Builds latem-ubuntu (README.md) with docker buildx.
#
#   ./build.sh           this machine's architecture, loaded into the local
#                        daemon as latem-ubuntu:local
#   ./build.sh --all     linux/amd64 and linux/arm64, built and left in the
#                        builder's cache: proves both build, publishes nothing
#   ./build.sh --push    linux/amd64 and linux/arm64, pushed to $IMAGE as
#                        :24.04 and :24.04-YYYYMMDD
#
# IMAGE defaults to ghcr.io/abyssinia-labs/latem-ubuntu. --push needs a prior
# `docker login ghcr.io` with a token that can write packages.
#
# The non-native architecture builds under QEMU and is several times slower:
# budget half an hour for --all or --push from a cold cache.

here=$(cd "$(dirname "$0")" && pwd)
image=${IMAGE:-ghcr.io/abyssinia-labs/latem-ubuntu}
builder=${BUILDER:-latem}
stamp=$(date -u +%Y%m%d)

mode=${1:-}
case $mode in
  "" | --all | --push) ;;
  *)
    echo "usage: $0 [--all | --push]" >&2
    exit 64
    ;;
esac

if [[ -z $mode ]]; then
  docker buildx build --load --tag latem-ubuntu:local "$here"
  echo "built latem-ubuntu:local — check it with $here/check.sh"
  exit 0
fi

# A daemon's own builder can only build multi-platform images when the daemon
# uses the containerd image store, which a stock Linux install does not. A
# docker-container builder can, so one is created on first use. It is a local
# BuildKit container and nothing else.
if ! docker buildx inspect "$builder" >/dev/null 2>&1; then
  docker buildx create --name "$builder" --driver docker-container >/dev/null
fi

# Captured and matched in the shell rather than piped to grep (amendment A37).
info=$(docker buildx inspect --bootstrap "$builder")
for p in linux/amd64 linux/arm64; do
  if [[ $info != *"$p"* ]]; then
    echo "builder '$builder' cannot build $p. Register QEMU for it once per boot:" >&2
    echo "  docker run --privileged --rm tonistiigi/binfmt --install ${p#linux/}" >&2
    exit 1
  fi
done

args=(--builder "$builder" --platform linux/amd64,linux/arm64)
if [[ $mode == --push ]]; then
  docker buildx build "${args[@]}" --push \
    --tag "$image:24.04" --tag "$image:24.04-$stamp" "$here"
  echo "pushed $image:24.04 and $image:24.04-$stamp"
  echo "pin default_image to the digest this prints:"
  echo "  docker buildx imagetools inspect $image:24.04-$stamp"
else
  docker buildx build "${args[@]}" --output type=cacheonly "$here"
  echo "linux/amd64 and linux/arm64 both build"
fi
