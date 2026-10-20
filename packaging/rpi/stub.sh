#!/bin/sh
# EdgeTAK @VERSION@ self-extracting installer for Raspberry Pi / Linux.
#
#   sudo sh edgetak-@VERSION@-rpi-installer.sh [--public-host HOST] [--no-firewall]
#   sudo sh edgetak-@VERSION@-rpi-installer.sh --uninstall [--purge]
#
# A gzip'd tar payload (install.sh + static binaries) follows the marker line.
set -eu
line=$(awk '/^__EDGETAK_PAYLOAD__$/ { print NR + 1; exit 0 }' "$0")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
tail -n +"$line" "$0" | tar -xzf - -C "$tmp"
EDGETAK_VERSION=@VERSION@ EDGETAK_INSTALLER=$(basename "$0") sh "$tmp/install.sh" "$@"
exit 0
__EDGETAK_PAYLOAD__
