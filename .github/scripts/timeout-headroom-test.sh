#!/usr/bin/env bash
# Self-test of timeout-headroom.sh on fixture logs (celeris#890). Run from the repository
# root; the Lint job runs it, so a change to the helper that stops warning above the
# threshold, reads the wrong package, or stops failing on a log with no result is a red Lint.
#
#   HEADROOM=path   the helper to test (default .github/scripts/timeout-headroom.sh; the
#                   negative control points it at a copy without the comparison and must
#                   see this test fail)
set -euo pipefail

helper=${HEADROOM:-.github/scripts/timeout-headroom.sh}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail=0

# check NAME LOG TIMEOUT WANT_STATUS WANT_WARNINGS WANT_STDOUT_SUBSTRING
check() {
  local name=$1 log=$2 timeout_s=$3 want_status=$4 want_warn=$5 want_out=$6 out status=0 warns
  out=$(GITHUB_STEP_SUMMARY="$dir/summary" "$helper" "$log" "$timeout_s" 2>"$dir/err") || status=$?
  warns=$(printf '%s\n' "$out" | grep -c '^::warning' || true)
  if [ "$status" -ne "$want_status" ] || [ "$warns" -ne "$want_warn" ]; then
    echo "FAIL $name: exit $status, $warns warning(s) (want exit $want_status, $want_warn warning(s))"
    printf '%s\n' "$out"
    cat "$dir/err"
    fail=1
    return
  fi
  if [ -n "$want_out" ] && ! printf '%s\n' "$out" | grep -qF -e "$want_out"; then
    echo "FAIL $name: output does not say '$want_out':"
    printf '%s\n' "$out"
    fail=1
    return
  fi
  echo "ok   $name"
}

printf '%s\n' \
  'ok  	github.com/goceleris/celeris/internal/conn	3.141s' \
  'ok  	github.com/goceleris/celeris/internal/engine/epoll	398.072s' \
  'ok  	github.com/goceleris/celeris/test/server	333.899s' >"$dir/root.log"
check "the slowest package is the one reported, below the threshold: no warning" "$dir/root.log" 600 0 0 'internal/engine/epoll took 398.1 s of its 600 s timeout: 66% of the timeout, 51% headroom'
check "74 per cent of the timeout is still below the threshold: no warning" "$dir/root.log" 540 0 0 '74% of the timeout'
# The threshold itself: 450.000 s of 600 s is exactly 75% and does not warn ("above 75%"),
# 450.001 s does.
printf '%s\n' 'ok  	github.com/goceleris/celeris/internal/engine/epoll	450.000s' >"$dir/edge.log"
check "exactly at the threshold: no warning" "$dir/edge.log" 600 0 0 '75% of the timeout'
printf '%s\n' 'ok  	github.com/goceleris/celeris/internal/engine/epoll	450.001s' >"$dir/edge-over.log"
check "a millisecond above the threshold: one warning" "$dir/edge-over.log" 600 0 1 '75% of the timeout'
check "above the threshold: one warning, the step still passes" "$dir/root.log" 500 0 1 '80% of the timeout'
rm -f "$dir/summary"
GITHUB_STEP_SUMMARY="$dir/summary" "$helper" "$dir/root.log" 500 >/dev/null
if [ "$(grep -c 'epoll took 398.1 s' "$dir/summary")" -ne 1 ]; then echo "FAIL the job summary does not carry the warning"; fail=1; else echo "ok   the warning goes to the job summary"; fi
if HEADROOM_WARN_PCT=60 "$helper" "$dir/root.log" 600 | grep -q '^::warning'; then echo "ok   HEADROOM_WARN_PCT lowers the threshold"; else echo "FAIL HEADROOM_WARN_PCT=60 does not warn at 66 per cent"; fail=1; fi

# A coverage run: the figure is followed by the coverage share.
printf '%s\n' 'ok  	github.com/goceleris/celeris/internal/engine/iouring	537.627s	coverage: 86.5% of statements' >"$dir/cover.log"
check "a coverage line is read" "$dir/cover.log" 780 0 0 'iouring took 537.6 s of its 780 s timeout: 69% of the timeout'
check "a coverage line above the threshold warns" "$dir/cover.log" 600 0 1 '90% of the timeout'

# Lines that are not a passing package's result are not read: a FAIL line, a test's own
# output and a package without test files.
printf '%s\n' \
  'FAIL	github.com/goceleris/celeris/internal/engine/iouring	999.000s' \
  '    foo_test.go:12: ok  github.com/x/y 999.000s' \
  '?   	github.com/goceleris/celeris/internal/bench	[no test files]' \
  'ok  	github.com/goceleris/celeris/internal/conn	2.000s' >"$dir/noise.log"
check "only passing results count" "$dir/noise.log" 600 0 0 'internal/conn took 2.0 s'

# Fail closed: a log with no result line.
printf '%s\n' '=== RUN   TestA' '--- PASS: TestA (0.00s)' 'PASS' >"$dir/none.log"
check "a log with no ok line fails" "$dir/none.log" 600 1 0 ''
: >"$dir/empty.log"
check "an empty log fails" "$dir/empty.log" 600 1 0 ''

# usage ARGS...: the helper must exit 2.
usage() {
  local name=$1 status=0
  shift
  "$helper" "$@" >/dev/null 2>&1 || status=$?
  if [ "$status" -ne 2 ]; then echo "FAIL $name exits $status, want 2"; fail=1; else echo "ok   $name exits 2"; fi
}
usage "a missing log" "$dir/missing.log" 600
usage "a zero timeout" "$dir/root.log" 0
usage "a timeout with a unit" "$dir/root.log" 600s
usage "a missing argument" "$dir/root.log"

exit "$fail"
