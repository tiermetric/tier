#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
	printf 'usage: %s <tag> <commit> <sha256> <out-file>\n' "$0" >&2
	exit 2
fi

tag=$1
commit=$2
sha256=$3
out=$4

if ! printf '%s\n' "$tag" | LC_ALL=C awk 'END { exit !(NR == 1 && $0 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) }'; then
	printf 'render-homebrew-formula: invalid tag: expected vMAJOR.MINOR.PATCH\n' >&2
	exit 2
fi
if ! printf '%s\n' "$commit" | LC_ALL=C awk 'END { exit !(NR == 1 && length($0) == 40 && $0 !~ /[^0-9a-f]/) }'; then
	printf 'render-homebrew-formula: invalid commit: expected 40 lowercase hex characters\n' >&2
	exit 2
fi
if ! printf '%s\n' "$sha256" | LC_ALL=C awk 'END { exit !(NR == 1 && length($0) == 64 && $0 !~ /[^0-9a-f]/) }'; then
	printf 'render-homebrew-formula: invalid sha256: expected 64 lowercase hex characters\n' >&2
	exit 2
fi

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmpl=$root/packaging/homebrew/tierd.rb.tmpl
if [ ! -r "$tmpl" ]; then
	printf 'render-homebrew-formula: template not readable: %s\n' "$tmpl" >&2
	exit 1
fi
tmp=$(mktemp "$out.tmp.XXXXXX")
trap 'rm -f "$tmp"' 0
trap 'exit 1' HUP INT TERM
sed -e "s/@@TAG@@/$tag/g" -e "s/@@COMMIT@@/$commit/g" -e "s/@@SHA256@@/$sha256/g" \
	"$tmpl" > "$tmp"

rc=0
grep -q '@@' "$tmp" || rc=$?
case "$rc" in
0)
	printf 'render-homebrew-formula: unresolved placeholder in output\n' >&2
	exit 1
	;;
1) ;;
*) exit "$rc" ;;
esac
mv "$tmp" "$out"
