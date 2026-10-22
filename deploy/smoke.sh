#!/bin/sh
# Runs inside the curl container of compose.smoke.yaml. Exits non-zero on the
# first failed assertion so `--exit-code-from smoke` fails the run.
set -eu

PROXY=http://edgecache:8080
ADMIN=http://edgecache:9101
ORIGIN=http://origin:8082

fail() { echo "FAIL: $*" >&2; exit 1; }

# Wait for readiness on the admin listener (same endpoint the Cloud Run probes hit).
i=0
until curl -fsS "$ADMIN/readyz" >/dev/null 2>&1; do
  i=$((i + 1)); [ "$i" -ge 50 ] && fail "edgecache never became ready"
  sleep 0.2
done
echo "readyz: $(curl -fsS "$ADMIN/readyz")"
echo "healthz: $(curl -fsS "$ADMIN/healthz")"

curl -fsS -X DELETE "$ORIGIN/admin/hits" >/dev/null

xcache() { curl -fsS -D - -o /dev/null "$PROXY$1" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-cache"{print $2}'; }

first=$(xcache /obj/42)
second=$(xcache /obj/42)
echo "GET /obj/42 #1 X-Cache: $first"
echo "GET /obj/42 #2 X-Cache: $second"
[ "$first" = "MISS" ] || fail "first request: want MISS, got '$first'"
[ "$second" = "HIT" ] || fail "second request: want HIT, got '$second'"

hits=$(curl -fsS "$ORIGIN/admin/hits")
echo "origin hits: $hits"
echo "$hits" | grep -q '"total":1' || fail "origin should have been hit once, got $hits"

echo "metrics:"
curl -fsS "$ADMIN/metrics" | grep -E '^edgecache_(requests_total|origin_requests_total|build_info)'

purge=$(curl -fsS -X POST -H 'X-Purge-Token: smoke-only-token' -d '{"prefix":"/obj/"}' "$ADMIN/admin/purge")
echo "purge: $purge"
third=$(xcache /obj/42)
echo "GET /obj/42 #3 (after purge) X-Cache: $third"
[ "$third" = "MISS" ] || fail "after purge: want MISS, got '$third'"

echo "SMOKE OK"
