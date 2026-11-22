#!/usr/bin/env bash
# Self-test of host-skip-reason.sh on fixture logs (celeris#756 CI fix). Run from the repository
# root; the Lint job runs it.
#
#   HOST_SKIP_REASON=path   the helper to test (default .github/scripts/host-skip-reason.sh)
set -euo pipefail

helper=${HOST_SKIP_REASON:-.github/scripts/host-skip-reason.sh}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail=0

# check NAME LOG WANT_STATUS WANT_STDOUT_SUBSTRING
check() {
  local name=$1 log=$2 want_status=$3 want_out=$4 out status=0
  out=$("$helper" "$log" 2>"$dir/err") || status=$?
  if [ "$status" -ne "$want_status" ] || ! grep -qF -e "$want_out" <<<"$out"; then
    echo "FAIL $name: exit $status, stdout '$out' (want exit $want_status and '$want_out')"
    fail=1
    return
  fi
  echo "ok   $name"
}

cat >"$dir/right.log" <<'LOG'
=== RUN   TestStartFDLEngineRidesOutRingENOMEM
    ring_enomem_start_linux_test.go:25: could not fill the memlock budget (512 rings held)
--- SKIP: TestStartFDLEngineRidesOutRingENOMEM (0.21s)
=== RUN   TestDialWatchDescribesTheSocketsOfAPort1229
    dial_watch_1229_linux_test.go:539: this host did not leave the dial to 10.255.255.1:37913 unanswered with a SYN_SENT row in the snapshot
--- SKIP: TestDialWatchDescribesTheSocketsOfAPort1229 (1.29s)
=== RUN   TestUnrelated
--- SKIP: TestUnrelated (0.00s)
LOG
check "the reason of each test passes (an unrelated skip is not this helper's)" "$dir/right.log" 0 "host-dependent skips: 2"

cat >"$dir/wrong.log" <<'LOG'
=== RUN   TestStartFDLEngineRidesOutRingENOMEM
    ring_enomem_start_linux_test.go:19: the engine does not run io_uring here (a kernel before 5.19, or CELERIS_MAX_IOURING_TIER capping the tier to none)
--- SKIP: TestStartFDLEngineRidesOutRingENOMEM (0.00s)
LOG
check "another reason of the same test fails" "$dir/wrong.log" 1 "NOT the host reason"

cat >"$dir/none.log" <<'LOG'
=== RUN   TestDialWatchDescribesTheSocketsOfAPort1229
--- SKIP: TestDialWatchDescribesTheSocketsOfAPort1229 (0.00s)
LOG
check "a skip with no reason line fails" "$dir/none.log" 1 "NOT the host reason"

cat >"$dir/other-test.log" <<'LOG'
=== RUN   TestStartFDLEngineRidesOutRingENOMEM
=== RUN   TestOther
    other_test.go:1: could not fill the memlock budget (512 rings held)
--- SKIP: TestStartFDLEngineRidesOutRingENOMEM (0.00s)
LOG
check "the reason printed by another test does not count" "$dir/other-test.log" 1 "NOT the host reason"

cat >"$dir/ran.log" <<'LOG'
=== RUN   TestStartFDLEngineRidesOutRingENOMEM
--- PASS: TestStartFDLEngineRidesOutRingENOMEM (0.60s)
LOG
check "a test that ran and passed has no skip to judge" "$dir/ran.log" 0 "host-dependent skips: 0"

if "$helper" "$dir/does-not-exist.log" >/dev/null 2>&1; then echo "FAIL an unreadable log must fail"; fail=1; else echo "ok   an unreadable log fails"; fi
echo "6 cases"
exit "$fail"
