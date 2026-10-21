#!/bin/sh
# Prints the release notes for a version from CHANGELOG.md, followed by a
# link comparing it with the previous release. Fails when the changelog has
# no section for the version, so a release can't go out without notes.
#   sh packaging/release-notes.sh v0.2.22 [previous-tag]
set -eu
version=$1
prev=${2:-}
cd "$(dirname "$0")/.."

notes=$(awk -v head="## $version " '
	index($0, head) == 1 { on = 1; next }
	on && /^## / { exit }
	on { print }
' CHANGELOG.md | sed -e '/./,$!d' | awk '
	# Join wrapped lines: GitHub shows line breaks in release notes as they
	# are. Indented lines continue the line above unless they start a list
	# item, follow a blank line or are inside a code block.
	/^ *```/ { flush(); print; fence = !fence; next }
	fence { print; next }
	/^ +[^ -]/ && has && buf != "" { sub(/^ +/, ""); buf = buf " " $0; next }
	{ flush(); buf = $0; has = 1 }
	END { flush() }
	function flush() { if (has) print buf; buf = ""; has = 0 }
')

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "CHANGELOG.md has no section for $version: add \"## $version (YYYY-MM-DD)\" before tagging" >&2
	exit 1
fi

printf '%s\n' "$notes"
if [ -n "$prev" ]; then
	printf '\n**Full changelog**: https://github.com/brandongotak/EdgeTAK/compare/%s...%s\n' "$prev" "$version"
fi
