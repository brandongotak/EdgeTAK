#!/bin/sh
# EdgeTAK one-line installer for Raspberry Pi OS and other systemd Linux.
#
#   curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh -s -- [options]
#
# Downloads the release installer, verifies its SHA-256 against the release's
# SHA256SUMS, keeps a copy in /usr/local/lib/edgetak (for later --uninstall)
# and runs it with the given options, e.g.:
#
#   ... | sudo sh -s -- --public-host tak.example.com --letsencrypt you@example.com --no-plaintext
#
# EDGETAK_VERSION=v0.2.0 selects a release (default: latest).
set -eu

REPO=brandongotak/EdgeTAK
VERSION=${EDGETAK_VERSION:-latest}
DEST=/usr/local/lib/edgetak

die() { printf 'edgetak install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root: curl -fsSL ... | sudo sh -s -- [options]"

if [ -n "${EDGETAK_BASE_URL:-}" ]; then
	base=$EDGETAK_BASE_URL # for testing against a local mirror
elif [ "$VERSION" = latest ]; then
	base=https://github.com/$REPO/releases/latest/download
else
	base=https://github.com/$REPO/releases/download/$VERSION
fi

fetch() { # url file
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "==> Downloading EdgeTAK ($VERSION)"
fetch "$base/edgetak-installer.sh" "$tmp/edgetak-installer.sh" || die "download failed: $base/edgetak-installer.sh"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"

want=$(awk '$2 == "edgetak-installer.sh" || $2 == "*edgetak-installer.sh" { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "SHA256SUMS has no entry for edgetak-installer.sh"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/edgetak-installer.sh" | awk '{print $1}')
else
	got=$(shasum -a 256 "$tmp/edgetak-installer.sh" | awk '{print $1}')
fi
[ "$got" = "$want" ] || die "checksum mismatch (got $got, want $want); not installing"
echo "==> Verified installer checksum"

mkdir -p "$DEST"
install -m 0755 "$tmp/edgetak-installer.sh" "$DEST/edgetak-installer.sh"

# The installer refers to itself in its help text (e.g. for --uninstall).
EDGETAK_INSTALLER="$DEST/edgetak-installer.sh" sh "$DEST/edgetak-installer.sh" "$@"
