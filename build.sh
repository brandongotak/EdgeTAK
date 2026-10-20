#!/bin/sh
# Cross-compiles EdgeTAK for common edge targets into dist/.
set -eu
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p dist
for target in linux/amd64 linux/arm64 linux/arm windows/amd64 darwin/arm64; do
	os=${target%/*}
	arch=${target#*/}
	out="dist/edgetak-$VERSION-$os-$arch"
	[ "$os" = windows ] && out="$out.exe"
	echo "building $out"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/edgetak
done
