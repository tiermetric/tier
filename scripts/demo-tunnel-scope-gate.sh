#!/usr/bin/env bash
#
# demo-tunnel-scope-gate.sh -- assert the demo's Cloudflare Named Tunnel routes
# EXACTLY ONE public hostname.
#
# WHY THIS EXISTS (#540, ruled Option A on 2026-08-01)
# ---------------------------------------------------------
# The demo's `cloudflared` sidecar ships with two known-unfixed HIGH advisories
# and there is no patched cloudflared release in existence, so the risk was
# ACCEPTED against a BLAST-RADIUS BOUND rather than a reachability argument:
#
#   "Total compromise of cloudflared buys an attacker the ability to serve a
#    fake-data page at demo.tiermetric.org, and nothing else."
#
# That sentence is only true if the tunnel routes ONE hostname. If infrastructure
# reuses an existing multi-hostname tunnel, the same compromise reaches every
# other hostname on it -- and the acceptance silently becomes false without any
# file changing. `deploy/DEMO.md` used to *instruct* a dedicated tunnel in prose;
# prose is not a control. This script is the assertion that makes the bound real.
#
# A reachability trace would have expired on the next advisory. This bound holds
# by SHAPE, so it survives cloudflared shipping new CVEs -- which it will.
#
# WHO RUNS IT
# -----------
# infrastructure-management: they own the Cloudflare account, mint the connector
# token, and are the acceptor of record. tier owns this assertion and the impact
# statement. Run it BEFORE the demo goes public, and again after ANY change to
# the tunnel's public-hostname list.
#
# USAGE
#   CF_API_TOKEN=<token> CF_ACCOUNT_ID=<id> CF_TUNNEL_ID=<uuid> \
#     scripts/demo-tunnel-scope-gate.sh [EXPECTED_HOSTNAME]
#
#   scripts/demo-tunnel-scope-gate.sh --selftest   # no credentials needed
#
# EXPECTED_HOSTNAME defaults to demo.tiermetric.org.
#
# The API token needs `Account > Cloudflare Tunnel > Read`. A read-only token is
# sufficient and is what should be used -- this gate must never be able to CHANGE
# the thing it audits.
#
# EXIT STATUS -- three outcomes, deliberately distinguished. An exit code alone
# never proves WHICH failure fired, and a harness that scores its own breakage as
# success is this project's most persistent bug class:
#   0 = tunnel routes exactly the expected hostname            -> bound HOLDS
#   1 = tunnel routes something else / more than one hostname  -> bound BROKEN
#   2 = could not run (missing env, no jq/curl, API error)     -> NOT a pass
#
# Never treat 2 as 0. "Could not check" is not "checked and clean".

set -euo pipefail

readonly DEFAULT_HOSTNAME='demo.tiermetric.org'

die_cannot_run() {
	printf 'demo-tunnel-scope-gate: CANNOT RUN: %s\n' "$1" >&2
	printf 'demo-tunnel-scope-gate: exiting 2 -- this is NOT a pass\n' >&2
	exit 2
}

# ---------------------------------------------------------------------------
# extract_hostnames <config-json>
#
# Emits one hostname per line from a tunnel configuration document.
#
# Cloudflare's ingress array always ends with a catch-all rule that has NO
# `hostname` key (typically `service: http_status:404`). That trailing rule is
# structural, not a route, so it must not count toward the total -- otherwise a
# correctly-scoped tunnel reads as two routes and the gate fails on good input.
# `select(has("hostname"))` drops it. Empty-string hostnames are dropped too:
# some configs carry `"hostname": ""` for the catch-all instead of omitting it,
# and an empty hostname is not a route either.
#
# ⚠️ MUTATION-TESTING NOTE, so nobody re-chases this: `select(has("hostname"))`
# is a KNOWN EQUIVALENT MUTANT -- deleting it does not change behaviour, because
# `.hostname` on an object lacking the key yields null, which the `!= null`
# filter then drops. It survives the matrix for that reason and NOT because the
# selftest is blind. Verified directly:
#   jq '... | map(.) | map(.hostname) | map(select(. != null and . != ""))'
# still yields exactly one hostname for a catch-all with no `hostname` key.
# It is kept because it states the intent (the catch-all is not a route) at the
# point where that intent is applied. The `!= ""` filter, by contrast, IS
# load-bearing and its deletion is caught.
# ---------------------------------------------------------------------------
# DISTINCT hostnames. `unique` matters: cloudflared allows several rules for the
# SAME hostname differing only by `path` (e.g. /api routed separately). Those are
# one public hostname, not several -- counting rules instead of hostnames turned
# a legitimate config into a scary security-flavoured FAIL, and the natural
# response to a gate that cries wolf is to stop believing it.
extract_hostnames() {
	jq -r '.result.config.ingress // []
	       | map(select(has("hostname")))
	       | map(.hostname)
	       | map(select(. != null and . != ""))
	       | unique
	       | .[]' <<<"$1"
}

# ---------------------------------------------------------------------------
# tunnel_config_source <config-json>
#
# Emits the tunnel's management mode: "cloudflare" (remotely managed, so the
# document we just fetched IS the one in force) or "local".
#
# 🔴 WHY THIS MATTERS: for a LOCALLY-managed tunnel, Cloudflare's own API docs
# say the connector routes from a `config.yml` on the origin machine, and
# `.result.config` is merely whatever was last pushed remotely -- possibly
# stale, possibly never applied. Auditing it would be auditing a GHOST: the gate
# would report on a document that does not govern any traffic.
#
# Today's compose runs `tunnel run` with TUNNEL_TOKEN and mounts no config, so
# the tunnel is remotely managed and this reads the authoritative document. But
# this gate claims to hold by SHAPE and survive drift -- and someone mounting a
# config.yml later is exactly that drift. Absent the check it would silently
# flip to auditing nothing while still printing "bound HOLDS".
#
# Defaults to "cloudflare" when the key is absent, because older API responses
# omit it; that default is asserted by a selftest arm rather than assumed.
# ---------------------------------------------------------------------------
tunnel_config_source() {
	jq -r '.result.source // "cloudflare"' <<<"$1"
}

# ---------------------------------------------------------------------------
# find_permissive_catchall <config-json>
#
# Emits the `service` of any hostname-less ingress rule that is NOT a terminal
# status responder. Empty output means the catch-all is benign.
#
# 🔴 WHY THIS EXISTS -- it closes a hole that the hostname count alone CANNOT see,
# and the hole made this gate pass the exact configuration it was written to
# forbid. A Cloudflare ingress rule with no `hostname` matches EVERYTHING. The
# benign form is the mandatory terminal rule (`service: http_status:404`). But a
# hostname-less rule pointing at the APP is a WILDCARD ROUTE: every hostname
# CNAME'd to <tunnel-id>.cfargotunnel.com is then served by this connector.
#
# Because extract_hostnames drops hostname-less rules, such a config counts as
# exactly ONE hostname and passed with "bound HOLDS". Measured before the fix:
#   ingress: [ {hostname: demo.tiermetric.org, service: app}, {service: app} ]
#   -> rc=0, "blast-radius bound HOLDS"   <-- WRONG, that tunnel serves anything
#
# The lesson is the one this repo keeps relearning: a guard that counts the
# things it recognises is blind to the thing it discards. The discarded set
# needs its own assertion.
# ---------------------------------------------------------------------------
find_permissive_catchall() {
	jq -r '.result.config.ingress // []
	       | map(select((has("hostname") | not) or .hostname == null or .hostname == ""))
	       | map(select(((.service // "") | startswith("http_status:")) | not))
	       | map(.service // "<no service>")
	       | .[]' <<<"$1"
}

# ---------------------------------------------------------------------------
# assert_scope <config-json> <expected-hostname>
#
# The whole gate, factored out so --selftest can exercise the REAL logic against
# planted fixtures rather than a reimplementation of it. A selftest that tests a
# copy of the logic proves nothing about the logic that ships.
# ---------------------------------------------------------------------------
assert_scope() {
	local config_json="$1" expected="$2"
	local -a hostnames=()
	local h raw src

	# 🔴 Capture jq's output into a VARIABLE, and check its status, before the
	# read loop. The obvious `while read … done < <(extract_hostnames …)` form
	# discards the producer's exit status entirely -- `pipefail` does not cover
	# process substitution -- so a malformed API response made jq die, yielded
	# zero lines, and the gate reported "routes NO public hostname", i.e. "bound
	# BROKEN". That is a WRONG VERDICT for a could-not-check, and it violates this
	# script's own 0/1/2 contract. Fails safe, but sends an operator hunting a
	# routing bug that does not exist.
	raw="$(extract_hostnames "$config_json" 2>/dev/null)" || {
		printf 'CANNOT RUN: the tunnel configuration could not be parsed (unexpected API shape).\n' >&2
		return 2
	}

	src="$(tunnel_config_source "$config_json" 2>/dev/null)" || {
		printf 'CANNOT RUN: could not read the tunnel configuration source.\n' >&2
		return 2
	}
	if [ "$src" != 'cloudflare' ]; then
		printf 'CANNOT RUN: tunnel is %s-managed, so this API document is NOT the config in force.\n' "$src" >&2
		printf '            A locally-managed connector routes from a config.yml on the origin host;\n' >&2
		printf '            what the API returns may be stale or never-applied. Auditing it would be\n' >&2
		printf '            auditing a ghost. Assert the scope on the origin host instead.\n' >&2
		return 2
	fi

	# The empty case is handled ONCE, here -- not with a per-line `[ -n "$h" ]`
	# guard inside the loop. A herestring of an empty string still yields one
	# empty line, so the loop alone would count a no-route config as one route.
	# Testing `$raw` instead keeps extract_hostnames the ONE place that decides
	# what counts as a route: a second per-line empty filter would be redundant
	# with jq's, and redundancy in a guard is not free -- measured, the duplicate
	# made deleting jq's empty-string filter SURVIVE the entire selftest, because
	# this loop silently covered for it. One filter, one place, one thing to test.
	if [ -n "$raw" ]; then
		while IFS= read -r h; do
			hostnames+=("$h")
		done <<<"$raw"
	fi

	local count=${#hostnames[@]}

	if [ "$count" -eq 0 ]; then
		printf 'FAIL: tunnel routes NO public hostname.\n' >&2
		printf '      Either the wrong tunnel id was given, or the route was never added.\n' >&2
		return 1
	fi

	if [ "$count" -ne 1 ]; then
		printf 'FAIL: tunnel routes %d public hostnames; the acceptance in #540 requires exactly 1.\n' "$count" >&2
		printf '      Routed: %s\n' "${hostnames[*]}" >&2
		printf '      A compromised cloudflared would reach ALL of these, not just the demo.\n' >&2
		printf '      Create a DEDICATED tunnel for the demo host instead of reusing this one.\n' >&2
		return 1
	fi

	if [ "${hostnames[0]}" != "$expected" ]; then
		printf 'FAIL: tunnel routes %s, expected %s.\n' "${hostnames[0]}" "$expected" >&2
		return 1
	fi

	# The hostname count is necessary but NOT sufficient: a hostname-less rule
	# matches everything, so a permissive catch-all is a wildcard route hiding
	# inside a config that counts as one hostname. Assert the DISCARDED set too.
	local permissive
	permissive="$(find_permissive_catchall "$config_json")" || return 2
	if [ -n "$permissive" ]; then
		printf 'FAIL: a hostname-less ingress rule routes to a real service, not a terminal status.\n' >&2
		printf '      Offending service(s): %s\n' "$(printf '%s' "$permissive" | tr '\n' ' ')" >&2
		printf '      A rule with no hostname matches EVERYTHING — any hostname pointed at this\n' >&2
		printf '      tunnel would be served. That is a wildcard route and it voids the #540 bound\n' >&2
		printf '      even though exactly one hostname is named. The final rule must be a terminal\n' >&2
		printf '      responder, i.e. service: http_status:404\n' >&2
		return 1
	fi

	printf 'OK: tunnel routes exactly one public hostname (%s) and its catch-all is terminal — #540 blast-radius bound HOLDS.\n' "$expected"
	return 0
}

# ---------------------------------------------------------------------------
# --selftest
#
# Control-armed: every arm that must FAIL is exercised, and the PASS arm is
# exercised too. A gate whose reject arm has never run is indistinguishable from
# a gate that accepts everything -- this repo has shipped that defect more than
# once.
# ---------------------------------------------------------------------------
run_selftest() {
	local pass=0 fail=0

	# check <name> <want_rc> <want_signature> <json> [expected-hostname]
	#
	# Asserts the exit code AND a signature substring of the message. Exit code
	# alone is not enough: several distinct failures all return 1, so an rc-only
	# assertion cannot tell WHICH branch fired. Mutation testing proved this
	# concretely -- deleting the zero-route check SURVIVED an rc-only selftest,
	# because a zero-route config then falls through to the count check and still
	# returns 1, just with a misleading message. This repo's standing rule:
	# "an exit code alone never proves which failure fired -- assert the signature."
	check() {
		local name="$1" want_rc="$2" want_sig="$3" json="$4" expected="${5:-$DEFAULT_HOSTNAME}"
		local got_rc=0 out
		out="$(assert_scope "$json" "$expected" 2>&1)" || got_rc=$?
		if [ "$got_rc" -ne "$want_rc" ]; then
			printf '  FAIL  %-52s (want rc=%d, got %d)\n' "$name" "$want_rc" "$got_rc"
			fail=$((fail + 1))
			return
		fi
		if [ -n "$want_sig" ] && [[ "$out" != *"$want_sig"* ]]; then
			printf '  FAIL  %-52s (rc ok, but message missing %q)\n' "$name" "$want_sig"
			printf '        got: %s\n' "$out"
			fail=$((fail + 1))
			return
		fi
		printf '  PASS  %-52s (rc=%d)\n' "$name" "$got_rc"
		pass=$((pass + 1))
	}

	printf 'demo-tunnel-scope-gate --selftest\n\n'

	# --- the ACCEPT arm: exactly one hostname + the structural catch-all -------
	check 'correctly scoped tunnel (+ catch-all)' 0 'bound HOLDS' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	# catch-all expressed as an EMPTY hostname rather than an absent key
	check 'catch-all with empty hostname string' 0 'bound HOLDS' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"hostname":"","service":"http_status:404"}]}}}'

	# --- REJECT arms: each must fail, and for its OWN stated reason ------------
	# The signature is what pins "own reason": several of these all return 1, so
	# without it the arms are interchangeable and a deleted branch survives.
	check 'REJECT: a second hostname on the same tunnel' 1 'routes 2 public hostnames' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"hostname":"other-service.example.com","service":"http://127.0.0.1:9000"},
		{"service":"http_status:404"}]}}}'

	check 'REJECT: right count, WRONG hostname' 1 'expected demo.tiermetric.org' '{"result":{"config":{"ingress":[
		{"hostname":"admin.example.com","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	check 'REJECT: no hostname routed at all' 1 'routes NO public hostname' '{"result":{"config":{"ingress":[
		{"service":"http_status:404"}]}}}'

	check 'REJECT: empty ingress' 1 'routes NO public hostname' '{"result":{"config":{"ingress":[]}}}'

	check 'REJECT: ingress key absent entirely' 1 'routes NO public hostname' '{"result":{"config":{}}}'

	# A wildcard is NOT a single hostname in any meaningful sense: it routes the
	# whole zone through this connector, which is the failure this gate exists to
	# catch, wearing a single-entry disguise. It must be rejected by the EQUALITY
	# branch, not merely by the count.
	check 'REJECT: wildcard hostname' 1 'expected demo.tiermetric.org' '{"result":{"config":{"ingress":[
		{"hostname":"*.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	# --- arms for the two holes BOTH reviewers found; neither was covered before,
	# --- and the first one PASSED with "bound HOLDS" until it was closed.
	check 'REJECT: catch-all points at the app (wildcard route)' 1 'hostname-less ingress rule routes to a real service' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http://127.0.0.1:8080"}]}}}'

	check 'REJECT: permissive rule mid-list, benign 404 last' 1 'hostname-less ingress rule routes to a real service' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http://10.0.0.5:9000"},
		{"service":"http_status:404"}]}}}'

	check 'CANNOT-RUN: locally-managed tunnel (config not in force)' 2 'NOT the config in force' '{"result":{"source":"local","config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	# The default-when-absent is ASSERTED, not assumed: older API responses omit
	# `source`, and defaulting the wrong way would make every such tunnel
	# un-auditable (rc 2 forever) or, worse, silently accepted.
	check 'source absent defaults to cloudflare (still passes)' 0 'bound HOLDS' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	check 'explicit source=cloudflare passes' 0 'bound HOLDS' '{"result":{"source":"cloudflare","config":{"ingress":[
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	# Several rules for ONE hostname differing by path are ONE public hostname.
	# Without `unique` this FAILED as "routes 2 public hostnames" -- a false
	# alarm on a legitimate config, which teaches operators to distrust the gate.
	check 'path-split rules on ONE hostname still pass' 0 'bound HOLDS' '{"result":{"config":{"ingress":[
		{"hostname":"demo.tiermetric.org","path":"/api","service":"http://127.0.0.1:8080"},
		{"hostname":"demo.tiermetric.org","service":"http://127.0.0.1:8080"},
		{"service":"http_status:404"}]}}}'

	printf '\n  %d passed, %d failed\n' "$pass" "$fail"
	if [ "$fail" -ne 0 ]; then
		printf '  SELFTEST FAILED — do not trust this gate until it is green.\n' >&2
		return 1
	fi
	printf '  selftest green: the reject arms genuinely reject and the accept arm genuinely accepts.\n'
	return 0
}

# ---------------------------------------------------------------------------
main() {
	case "${1:-}" in
	--selftest)
		command -v jq >/dev/null 2>&1 || die_cannot_run 'jq is not installed'
		# `|| rc=$?` is REQUIRED: a bare `run_selftest` under `set -e` aborts on a
		# non-zero return, so a trailing `exit $?` would be dead code that merely
		# LOOKS like the mechanism -- and would mislead whoever next changes the
		# selftest's exit contract.
		local rc=0
		run_selftest || rc=$?
		exit "$rc"
		;;
	-h | --help)
		# Without this, `--help` is taken as EXPECTED_HOSTNAME and the script
		# attempts a live API call against a hostname called "--help".
		sed -n '2,52p' "$0" | sed 's/^#\{1,2\} \{0,1\}//'
		exit 0
		;;
	esac

	local expected="${1:-$DEFAULT_HOSTNAME}"

	command -v curl >/dev/null 2>&1 || die_cannot_run 'curl is not installed'
	command -v jq >/dev/null 2>&1 || die_cannot_run 'jq is not installed'
	[ -n "${CF_API_TOKEN:-}" ] || die_cannot_run 'CF_API_TOKEN is not set'
	[ -n "${CF_ACCOUNT_ID:-}" ] || die_cannot_run 'CF_ACCOUNT_ID is not set'
	[ -n "${CF_TUNNEL_ID:-}" ] || die_cannot_run 'CF_TUNNEL_ID is not set'

	local url body http_code
	url="https://api.cloudflare.com/client/v4/accounts/${CF_ACCOUNT_ID}/cfd_tunnel/${CF_TUNNEL_ID}/configurations"

	# -w writes the status on its own trailing line. Do NOT pipe curl into
	# anything here: a pipeline reports the LAST command's status, so a failed
	# curl would be masked by a successful filter -- the exact trap that voided a
	# control arm in this repo on 2026-07-30.
	# `curl_err` keeps -S's diagnostic instead of discarding it: pairing -sS with
	# 2>/dev/null throws away the exact message that explains an rc=2.
	local curl_err
	curl_err="$(mktemp)"
	body="$(curl -sS -w $'\n%{http_code}' \
		-H "Authorization: Bearer ${CF_API_TOKEN}" \
		-H 'Content-Type: application/json' \
		"$url" 2>"$curl_err")" || {
		printf 'demo-tunnel-scope-gate: curl: %s\n' "$(cat "$curl_err")" >&2
		rm -f "$curl_err"
		die_cannot_run 'the Cloudflare API request failed (network/TLS)'
	}
	rm -f "$curl_err"

	http_code="${body##*$'\n'}"
	body="${body%$'\n'*}"

	[ "$http_code" = '200' ] || die_cannot_run "the Cloudflare API returned HTTP ${http_code} (check the token scope: Account > Cloudflare Tunnel > Read)"

	jq -e '.success == true' >/dev/null 2>&1 <<<"$body" \
		|| die_cannot_run 'the Cloudflare API reported success=false'

	assert_scope "$body" "$expected"
}

main "$@"
