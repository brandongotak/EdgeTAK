#!/bin/sh
# Builds dist/edgetak-<version>-rpi-installer.sh: a single self-extracting
# installer with static binaries for 64-bit Pi OS, 32-bit Pi OS (all Pis,
# including Zero/1) and x86-64 Linux.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
VERSION=${VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/payload/bin" "$ROOT/dist"

build() { # goarch goarm name
	echo "building linux/$1${2:+ (GOARM=$2)}"
	(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$1 GOARM=$2 \
		go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$work/payload/bin/edgetak-linux-$3" ./cmd/edgetak)
}
build arm64 "" arm64
build arm 6 armv6
build amd64 "" amd64

cp "$HERE/install.sh" "$work/payload/install.sh"
tar -C "$work/payload" -czf "$work/payload.tgz" .

out="$ROOT/dist/edgetak-$VERSION-rpi-installer.sh"
{ sed "s/@VERSION@/$VERSION/g" "$HERE/stub.sh"; cat "$work/payload.tgz"; } >"$out"
chmod +x "$out"
echo "wrote $out ($(wc -c <"$out" | tr -d ' ') bytes)"
