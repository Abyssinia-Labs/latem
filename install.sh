#!/bin/sh
#
# Latem installer.  curl -fsSL https://.../install.sh | sh
#
# Detects the platform, downloads the matching binary, verifies its checksum,
# installs it to ~/.local/bin with `ltm` beside it, and runs `latem doctor`.
#
# It never runs as root, and that is a requirement rather than a preference
# (RUNNER_SPEC §23). The agent must run as a logged-in user: on macOS a
# LaunchAgent is the only context where `codesign` can reach the login
# keychain, and on Linux a systemd *user* unit keeps the build's files owned by
# the person who will have to debug them. An installer that asks for sudo
# teaches the wrong shape from the first command.
#
# POSIX sh — no bashisms — because /bin/sh is dash on Debian and Ubuntu, and
# `curl | sh` runs under whatever /bin/sh happens to be.

set -eu

REPO="Abyssinia-Labs/latem"
VERSION="${LATEM_VERSION:-latest}"
INSTALL_DIR="${LATEM_INSTALL_DIR:-$HOME/.local/bin}"
BASE_URL="${LATEM_BASE_URL:-https://github.com/$REPO/releases/download}"

# The release signing key, as `minisign -G` printed it. Empty means this
# repository has not generated one yet, and the signature check below is
# skipped with a warning rather than silently.
#
# It is the same key as internal/selfupdate/trusted.pub, which is the copy the
# agent embeds and verifies its own updates against. Two copies of a key that
# have to agree is a drift waiting to happen, so a test asserts they do
# (test/scripts/install_test.go, amendment A37's rule about hazards that can
# recur).
#
# It has to be the key itself and cannot be a path to that file, for two
# reasons worth writing down because the path is the obvious thing to reach
# for. This script is fetched with curl and piped into sh, so there is no
# checkout on the machine running it and nothing at any repository-relative
# path. And `minisign -P` takes the base64 key; the file form is `-p`, so a
# path here fails with "base64 conversion failed - was an actual public key
# given?" even where the file does exist.
PUBKEY="RWTTZK1Cxjg51w6BUpg5KpkKhOHzUT+EDhgidVqHUHsiTr6nPjKC3esu"

# ---------------------------------------------------------------- output

# Colour only when stdout is a terminal. A `curl | sh` transcript pasted into
# an issue should be readable, not full of escape sequences.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	BOLD=$(printf '\033[1m')
	RED=$(printf '\033[31m')
	YELLOW=$(printf '\033[33m')
	RESET=$(printf '\033[0m')
else
	BOLD='' RED='' YELLOW='' RESET=''
fi

info() { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*"; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die() {
	printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2
	exit 1
}

# ------------------------------------------------------------ preconditions

# Refuse root before doing anything else, including the download.
#
# The failure this prevents is not hypothetical: a binary installed to
# /root/.local/bin and a LaunchDaemon registered as root produce a machine that
# looks installed and cannot sign anything, with errors that name codesign
# rather than the install.
if [ "$(id -u)" -eq 0 ]; then
	die "do not run this as root.

Latem's agent runs as a logged-in user — that is what gives it a keychain on
macOS and sane file ownership on Linux (RUNNER_SPEC §23). Run this again as
the account that should own the builds:

  curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sh"
fi

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}
need uname
need mkdir
need chmod

if command -v curl >/dev/null 2>&1; then
	FETCH='curl -fsSL --retry 3 --retry-delay 1 -o'
	FETCH_STDOUT='curl -fsSL --retry 3 --retry-delay 1'
elif command -v wget >/dev/null 2>&1; then
	FETCH='wget -q -O'
	FETCH_STDOUT='wget -q -O -'
else
	die "neither curl nor wget is installed"
fi

# ------------------------------------------------------------- platform

step "Detecting platform"

os=$(uname -s)
case "$os" in
Darwin) OS=darwin ;;
Linux) OS=linux ;;
*) die "unsupported operating system: $os (Latem targets macOS and Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) ARCH=amd64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*) die "unsupported architecture: $arch (Latem targets amd64 and arm64)" ;;
esac

info "  $OS/$ARCH"

# ------------------------------------------------------------- version

if [ "$VERSION" = "latest" ]; then
	step "Resolving the latest release"
	# api.github.com, whose answer to /releases/latest excludes drafts and
	# prereleases. It needs no token, and it is rate-limited by IP (60 an hour
	# unauthenticated), which a machine behind a shared NAT can exhaust without
	# having run this script at all. LATEM_VERSION is the way past that, and
	# the error below says so.
	VERSION=$($FETCH_STDOUT "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -n 1 || true)
	[ -n "$VERSION" ] || die "could not determine the latest release; set LATEM_VERSION explicitly"
fi
info "  $VERSION"

ASSET="latem_${VERSION#v}_${OS}_${ARCH}.tar.gz"
URL="$BASE_URL/$VERSION/$ASSET"
SUMS_URL="$BASE_URL/$VERSION/latem_${VERSION#v}_checksums.txt"

# --------------------------------------------------------------- download

TMP=$(mktemp -d "${TMPDIR:-/tmp}/latem-install.XXXXXX")
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

step "Downloading $ASSET"
$FETCH "$TMP/$ASSET" "$URL" || die "download failed: $URL"

# ---------------------------------------------------------------- verify

# The checksum is not optional and there is no flag to skip it. This script is
# piped into a shell over the network, and "carry on and see" is not an
# acceptable answer to a damaged download.
#
# Be precise about what it proves, though, because the stronger claim is the
# tempting one: the checksums file comes from the same place as the archive, so
# a sha256 match means the file arrived intact from whoever served it. It says
# nothing about who that was. The minisign signature is what says that, and it
# is checked below when `minisign` is installed — which is also exactly what an
# agent with auto_update does for itself, on every release, with the key
# compiled into it (RUNNER_SPEC §20).
step "Verifying the download"
$FETCH "$TMP/checksums.txt" "$SUMS_URL" || die "could not download checksums: $SUMS_URL"

# The signature first, then the manifest it authenticates, then the archive
# that manifest names. In that order, because a checksum read out of a file
# whose signature has not been checked is a checksum from whoever served it.
if [ -z "$PUBKEY" ]; then
	warn "this installer carries no release signing key, so nothing here proves
who built the binary — only that it arrived intact. That is a gap in this
repository rather than in your install; see docs/release.md."
elif ! command -v minisign >/dev/null 2>&1; then
	# Not fatal. Requiring minisign would turn a one-line install into a
	# package-manager detour on every machine, and the checksum still catches
	# every damaged download. Say plainly what was not checked, and how to.
	warn "minisign is not installed, so the release signature was not checked.
The checksum still proves this download is intact, not who produced it. To
check the rest by hand:

  brew install minisign        # or: sudo apt-get install minisign
  minisign -V -P '$PUBKEY' -m latem_${VERSION#v}_checksums.txt"
else
	$FETCH "$TMP/checksums.txt.minisig" "$SUMS_URL.minisig" ||
		die "could not download the signature: $SUMS_URL.minisig

Every release carries one. A release that does not is not one to install."
	# minisign's own words are kept and printed on failure. Discarding them
	# was the first version of this, and it makes two very different failures
	# look identical: a signature that does not verify, and a PUBKEY above that
	# minisign could not load at all ("base64 conversion failed — was an actual
	# public key given?", which is what a path rather than a key produces).
	# The first is somebody else's problem and the second is ours.
	if ! sigout=$(minisign -V -P "$PUBKEY" \
		-m "$TMP/checksums.txt" \
		-x "$TMP/checksums.txt.minisig" 2>&1); then
		die "the checksums file is not signed by Latem's release key.

minisign said:
  $sigout

Do not use this download. Either it was altered in transit, or the release was
signed with a key this installer does not know — report it at
https://github.com/$REPO/issues"
	fi
	info "  signature ok"
fi

expected=$(grep " $ASSET\$" "$TMP/checksums.txt" | awk '{print $1}' | head -n 1)
[ -n "$expected" ] || die "$ASSET is not listed in the checksum file"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
else
	die "neither sha256sum nor shasum is available; cannot verify the download"
fi

if [ "$expected" != "$actual" ]; then
	die "checksum mismatch for $ASSET
  expected $expected
  got      $actual

Do not use this file. Report it at https://github.com/$REPO/issues"
fi
info "  checksum ok"

# --------------------------------------------------------------- install

step "Installing to $INSTALL_DIR"
need tar
tar -xzf "$TMP/$ASSET" -C "$TMP" || die "could not extract $ASSET"
[ -f "$TMP/latem" ] || die "the archive does not contain a latem binary"

mkdir -p "$INSTALL_DIR"
# Install to a temporary name in the destination directory and rename, so a
# concurrent `latem` keeps running from its own inode and no window exists
# where the binary is half-written.
mv "$TMP/latem" "$INSTALL_DIR/.latem.new"
chmod 0755 "$INSTALL_DIR/.latem.new"
mv "$INSTALL_DIR/.latem.new" "$INSTALL_DIR/latem"
info "  $INSTALL_DIR/latem"

# ltm is latem's short name: the same binary, by a link beside it (A79). A file
# called ltm that is anything else is somebody else's, and stays.
if [ -L "$INSTALL_DIR/ltm" ] && [ "$(readlink "$INSTALL_DIR/ltm")" = latem ]; then
	info "  $INSTALL_DIR/ltm (a link to latem)"
elif [ -e "$INSTALL_DIR/ltm" ] || [ -L "$INSTALL_DIR/ltm" ]; then
	warn "$INSTALL_DIR/ltm is not latem's, so it was left alone; the short name ltm is not installed"
else
	ln -s latem "$INSTALL_DIR/ltm"
	info "  $INSTALL_DIR/ltm (a link to latem)"
fi

# macOS quarantines anything downloaded through a browser or curl and refuses
# to execute it with a dialog nobody sees over ssh.
if [ "$OS" = darwin ] && command -v xattr >/dev/null 2>&1; then
	xattr -d com.apple.quarantine "$INSTALL_DIR/latem" 2>/dev/null || true
fi

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*)
	warn "$INSTALL_DIR is not on your PATH. Add it:

  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile
  export PATH=\"\$HOME/.local/bin:\$PATH\""
	;;
esac

# ---------------------------------------------------------------- doctor

# Running doctor here is the whole point of the installer having a second half.
# Almost every failure mode of a self-hosted runner is environmental, and the
# cheapest moment to find out is before anyone has written a jobs.yaml.
#
# Its non-zero exit is expected on a machine with no config yet and must not
# fail the install — the operator is meant to read the list, not be handed an
# error.
step "Checking this machine"
info ""
"$INSTALL_DIR/latem" doctor || true

cat <<EOF

${BOLD}Installed.${RESET} Next:

  latem setup                          config, registration (approved in your
                                       browser), the service, and doctor
  latem repo add <owner/name>          allow a repository
  latem workflows check <checkout>     what this machine can run of it
  latem secrets set NAME               a value its workflows read

Building locally only, with no control plane: latem setup --no-register

Docs: https://github.com/$REPO
EOF
