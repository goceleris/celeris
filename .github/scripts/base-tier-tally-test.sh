#!/usr/bin/env bash
# Self-test of base-tier-tally.sh on fixture logs (celeris#890 item 87). Run from
# the repository root; the Lint job runs it, so a change to the tally that stops
# it failing on a skip nobody allowed, on a cap that did not take effect or on a
# test that is missing is a red Lint.
#
#   BASE_TIER_TALLY=path   the tally to test (default .github/scripts/base-tier-tally.sh)
set -uo pipefail

tally=${BASE_TIER_TALLY:-.github/scripts/base-tier-tally.sh}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail=0
n=0
export BASE_TIER_SKIPS_DIR=$dir
printf '# header\nmin-pass 2\nTestNeedsHighTier needs a provided buffer ring\n' >"$dir/base-tier-skips-iouring.txt"
printf '# header\nmin-pass 1\npattern:^.+/(io_uring|adaptive)$ the arms of probe-gated tests\nTestNeedsHighTier needs a provided buffer ring\n' >"$dir/base-tier-skips-root.txt"

# expect NAME WANT_STATUS WANT_STDERR_SUBSTRING LABEL LOG [TESTS...]
expect() {
  local name=$1 want=$2 sub=$3 status=0
  shift 3
  "$tally" "$@" >"$dir/out" 2>"$dir/err" || status=$?
  n=$((n + 1))
  if [ "$status" -ne "$want" ]; then
    echo "FAIL $name: exit $status, want $want"; cat "$dir/out" "$dir/err"; fail=1
  elif [ -n "$sub" ] && ! grep -qF -e "$sub" "$dir/err" "$dir/out"; then
    echo "FAIL $name: output does not say '$sub':"; cat "$dir/out" "$dir/err"; fail=1
  else
    echo "ok   $name"
  fi
}

# good.log: the shape of an iouring log at the base tier. 806 and the tier test passed,
# one allowed test skipped (with a subtest).
good() {
  printf '%s\n' \
    '=== RUN   TestShutdownDetachedPartialWriteIsFlushed806' '--- PASS: TestShutdownDetachedPartialWriteIsFlushed806 (0.10s)' \
    '=== RUN   TestReportTier844' \
    '    async_abort_844_engine_linux_test.go:160: celeris844 PROBE kernel=6.11.0 io_uring_tier=base CELERIS_MAX_IOURING_TIER="base"' \
    '--- PASS: TestReportTier844 (0.00s)' \
    '=== RUN   TestNeedsHighTier' '=== RUN   TestNeedsHighTier/sub' '    --- SKIP: TestNeedsHighTier/sub (0.00s)' '--- SKIP: TestNeedsHighTier (0.00s)' \
    '    async_abort_844_linux_test.go:518: celeris844 RESULT engine=io_uring tier=base arm=park inject=true' \
    'PASS'
}
good >"$dir/good.log"
expect "good iouring log" 0 "" iouring "$dir/good.log" TestShutdownDetachedPartialWriteIsFlushed806 TestReportTier844
sed 's/NeedsHighTier/NeedsSomething/' "$dir/good.log" >"$dir/unlisted.log"
expect "a skip nobody allowed fails" 1 "TestNeedsSomething" iouring "$dir/unlisted.log" TestShutdownDetachedPartialWriteIsFlushed806
sed 's/tier=base arm=park/tier=high arm=park/' "$dir/good.log" >"$dir/high.log"
expect "an arm at tier=high fails" 1 "the cap did not take effect" iouring "$dir/high.log" TestShutdownDetachedPartialWriteIsFlushed806
grep -v 'celeris844 PROBE' "$dir/good.log" >"$dir/noprobe.log"
expect "a missing capped probe line fails" 1 "the cap did not take effect" iouring "$dir/noprobe.log" TestShutdownDetachedPartialWriteIsFlushed806
sed 's/CELERIS_MAX_IOURING_TIER="base"/CELERIS_MAX_IOURING_TIER=""/' "$dir/good.log" >"$dir/nocap.log"
expect "a probe line of an uncapped run fails" 1 "the cap did not take effect" iouring "$dir/nocap.log" TestShutdownDetachedPartialWriteIsFlushed806
grep -v 'TestShutdownDetachedPartialWriteIsFlushed806' "$dir/good.log" >"$dir/no806.log"
expect "a named test that never ran fails" 1 "TestShutdownDetachedPartialWriteIsFlushed806 ran 0" iouring "$dir/no806.log" TestShutdownDetachedPartialWriteIsFlushed806
sed 's/^--- PASS: TestShutdownDetachedPartialWriteIsFlushed806/--- SKIP: TestShutdownDetachedPartialWriteIsFlushed806/' "$dir/good.log" >"$dir/skip806.log"
expect "a named test that skipped fails" 1 "TestShutdownDetachedPartialWriteIsFlushed806" iouring "$dir/skip806.log" TestShutdownDetachedPartialWriteIsFlushed806
# min-pass: a run that lost its tests (a -run pattern that stopped matching).
grep -v 'TestShutdownDetachedPartialWriteIsFlushed806' "$dir/good.log" | sed 's/^--- PASS: TestReportTier844/--- XXX: TestReportTier844/' >"$dir/shrunk.log"
expect "fewer top-level PASS lines than min-pass fails" 1 "fewer than the 2" iouring "$dir/shrunk.log"
# The root label has no tier lines. A pattern entry allows an arm only while the log
# shows tier=base under that arm's RUN; a top-level test, or an arm that skips for
# another reason, is not allowed by it.
printf '%s\n' '=== RUN   TestA' '--- PASS: TestA (0.00s)' 'PASS' >"$dir/root.log"
expect "root: a clean log" 0 "" root "$dir/root.log"
printf '%s\n' '=== RUN   TestA' '--- SKIP: TestA (0.00s)' '--- PASS: TestB (0.00s)' 'PASS' >"$dir/rootskip.log"
expect "root: a skip no entry names fails" 1 "TestA" root "$dir/rootskip.log"
arm() { # arm NAME MESSAGE: a probe-gated test whose NAME arm skipped with MESSAGE
  printf '%s\n' '=== RUN   TestP' "=== RUN   TestP/$1" "    p_test.go:9: $2" '=== RUN   TestP/std' '--- PASS: TestP (0.00s)' \
    "    --- SKIP: TestP/$1 (0.00s)" '    --- PASS: TestP/std (0.00s)' 'PASS'
}
arm io_uring 'io_uring tier=base kernel=6.17: no usable io_uring' >"$dir/arm.log"
expect "root: an arm that logged tier=base is allowed by the pattern" 0 "" root "$dir/arm.log"
arm io_uring 'the kernel has no io_uring' >"$dir/arm-other.log"
expect "root: an arm that skipped for another reason fails" 1 "TestP/io_uring" root "$dir/arm-other.log"
arm epoll 'io_uring tier=base kernel=6.17: no usable io_uring' >"$dir/arm-epoll.log"
expect "root: an arm the pattern does not name fails" 1 "TestP/epoll" root "$dir/arm-epoll.log"
printf '%s\n' '=== RUN   io_uring' '    p_test.go:9: io_uring tier=base: no usable io_uring' '--- SKIP: io_uring (0.00s)' '--- PASS: TestB (0.00s)' >"$dir/arm-top.log"
expect "root: a top-level test named like an arm fails" 1 "SKIP: io_uring" root "$dir/arm-top.log"
expect "a label that is neither fails with usage" 2 "neither iouring nor root" other "$dir/root.log"
expect "an unreadable log is a usage error" 2 "cannot read" root "$dir/absent.log"

echo "$n cases"
exit "$fail"
