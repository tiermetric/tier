#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
render=$root/scripts/render-homebrew-formula.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/homebrew-selftest.XXXXXX")
trap 'rm -rf "$work"' 0
trap 'exit 1' HUP INT TERM
commit=4a0b1a2de6c102945d01fa90b7a98b322d0409c2
sha256=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

"$render" v1.2.3 "$commit" "$sha256" "$work/tierd.rb"
grep -Fx '  url "https://github.com/tiermetric/tier/archive/refs/tags/v1.2.3.tar.gz"' "$work/tierd.rb"
grep -Fx "  sha256 \"$sha256\"" "$work/tierd.rb"
grep -Fx "    ldflags = \"-s -w -X main.version=v1.2.3 -X main.commit=$commit\"" "$work/tierd.rb"
rc=0
grep -q '@@' "$work/tierd.rb" || rc=$?
[ "$rc" -eq 1 ] || { echo 'FAIL good render: placeholders remain or output unreadable' >&2; exit 1; }
rc=0
grep -q '^  head ' "$work/tierd.rb" || rc=$?
[ "$rc" -eq 1 ] || { echo 'FAIL good render: head stanza present or output unreadable' >&2; exit 1; }
echo 'PASS good render: exact URL, checksum, version, commit, no placeholders or head stanza'

reject() {
	name=$1
	tag=$2
	revision=$3
	sha=$4
	marker=$5
	rc=0
	"$render" "$tag" "$revision" "$sha" "$work/rejected.rb" >"$work/stdout" 2>"$work/stderr" || rc=$?
	if [ "$rc" -ne 2 ] || ! grep -F "$marker" "$work/stderr" >/dev/null; then
		printf 'FAIL %s: wanted exit 2 and %s, got exit %s\n' "$name" "$marker" "$rc" >&2
		cat "$work/stderr" >&2
		exit 1
	fi
	printf 'PASS %s: exit 2, %s\n' "$name" "$marker"
}

for tag in 1.2.3 v1.2 v1.2.3-rc1 'v1.2.3/'; do
	reject 'bad tag' "$tag" "$commit" "$sha256" 'invalid tag:'
done
reject 'multiline tag' "$(printf 'v1.2.3\nv4.5.6')" "$commit" "$sha256" 'invalid tag:'
for revision in '' abc "${commit}0" 4A0b1a2de6c102945d01fa90b7a98b322d0409c2; do
	reject 'bad commit' v1.2.3 "$revision" "$sha256" 'invalid commit:'
done
reject 'multiline commit' v1.2.3 "$(printf '%s\n%s' "$commit" "$commit")" "$sha256" 'invalid commit:'
for sha in abc "${sha256}0" 0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef; do
	reject 'bad sha256' v1.2.3 "$commit" "$sha" 'invalid sha256:'
done
reject 'multiline sha256' v1.2.3 "$commit" "$(printf '%s\n%s' "$sha256" "$sha256")" 'invalid sha256:'

rc=0
"$render" v1.2.3 "$sha256" "$work/missing.rb" 2>"$work/stderr" || rc=$?
if [ "$rc" -ne 2 ] || ! grep -F '<tag> <commit> <sha256> <out-file>' "$work/stderr" >/dev/null; then
	echo 'FAIL missing commit argument: wanted exit 2 and four-argument usage' >&2
	exit 1
fi
echo 'PASS missing commit argument: exit 2, four-argument usage'

# Missing templates must fail without creating output.
mkdir -p "$work/repo/scripts" "$work/repo/packaging/homebrew"
cp "$render" "$work/repo/scripts/"
rc=0
"$work/repo/scripts/render-homebrew-formula.sh" v1.2.3 "$commit" "$sha256" "$work/missing-template.rb" 2>"$work/stderr" || rc=$?
if [ "$rc" -ne 1 ] || ! grep -F 'template not readable' "$work/stderr" >/dev/null || [ -e "$work/missing-template.rb" ]; then
	echo 'FAIL missing template: wanted exit 1, template not readable diagnostic, and no output' >&2
	exit 1
fi
echo 'PASS missing template: exit 1, template not readable, no output'

# Unknown template tokens must fail instead of shipping an incomplete formula.
cp "$root/packaging/homebrew/tierd.rb.tmpl" "$work/repo/packaging/homebrew/"
printf '\n@@UNKNOWN@@\n' >> "$work/repo/packaging/homebrew/tierd.rb.tmpl"
rc=0
"$work/repo/scripts/render-homebrew-formula.sh" v1.2.3 "$commit" "$sha256" "$work/unknown.rb" 2>"$work/stderr" || rc=$?
if [ "$rc" -ne 1 ] || ! grep -F 'unresolved placeholder' "$work/stderr" >/dev/null || [ -e "$work/unknown.rb" ]; then
	echo 'FAIL unknown placeholder: wanted exit 1, unresolved placeholder diagnostic, and no output' >&2
	exit 1
fi
echo 'PASS unknown placeholder: exit 1, unresolved placeholder, no output'
