#!/usr/bin/env bash
# Self-test of skip-count.sh on fixture logs (celeris#890). Run from the
# repository root; the Lint job runs it, so a change to the helper that makes
# it count wrongly, or stop failing on a joined line, is a red Lint.
#
#   SKIP_COUNT=path   the helper to test (default .github/scripts/skip-count.sh;
#                     the negative control points it at a copy without the
#                     comparison and must see this test fail)
set -euo pipefail

helper=${SKIP_COUNT:-.github/scripts/skip-count.sh}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail=0

# check NAME LOG WANT_STATUS WANT_COUNT [WANT_STDERR_SUBSTRING]
check() {
  local name=$1 log=$2 want_status=$3 want_count=$4 want_err=${5:-} out status=0
  out=$("$helper" "$log" 2>"$dir/err") || status=$?
  if [ "$status" -ne "$want_status" ] || [ "$out" != "$want_count" ]; then
    echo "FAIL $name: exit $status, stdout '$out' (want exit $want_status, stdout '$want_count')"
    fail=1
    return
  fi
  if [ -n "$want_err" ] && ! grep -qF -e "$want_err" "$dir/err"; then
    echo "FAIL $name: stderr does not list '$want_err':"
    cat "$dir/err"
    fail=1
    return
  fi
  echo "ok   $name"
}

# A log with a top-level skip, an indented subtest skip and passes.
printf '%s\n' \
  '=== RUN   TestA' '--- SKIP: TestA (0.00s)' \
  '=== RUN   TestB' '=== RUN   TestB/sub' '    --- SKIP: TestB/sub (0.00s)' '--- PASS: TestB (0.00s)' \
  'PASS' >"$dir/clean.log"
check "anchored lines counted, subtests included" "$dir/clean.log" 0 2

printf '%s\n' '=== RUN   TestA' '--- PASS: TestA (0.00s)' 'PASS' >"$dir/none.log"
check "a log with no skip counts 0" "$dir/none.log" 0 0

# The failure the helper exists for: output without a trailing newline in
# front of the result line.
printf '%s\n' '=== RUN   TestA' '--- SKIP: TestA (0.00s)' >"$dir/joined.log"
printf 'x--- SKIP: TestJoined (0.00s)\n' >>"$dir/joined.log"
check "a joined SKIP line fails and is listed" "$dir/joined.log" 1 "" 'x--- SKIP: TestJoined'
# The old tally on the same log: it counts 1 where there are 2 SKIP results.
old=$(grep -cE '^[[:space:]]*--- SKIP: ' "$dir/joined.log" || true)
if [ "$old" -ne 1 ]; then
  echo "FAIL fixture: the unanchored-blind grep counts $old, want 1"
  fail=1
fi
printf 'x--- SKIP: T (0.00s)\n' >"$dir/only-joined.log"
old=$(grep -cE '^[[:space:]]*--- SKIP: ' "$dir/only-joined.log" || true)
if [ "$old" -ne 0 ]; then
  echo "FAIL fixture: the old grep counts $old on a log of one joined line, want 0"
  fail=1
fi
check "a log of only a joined SKIP line fails" "$dir/only-joined.log" 1 ""

# A pintest child's output is logged with every line prefixed "child| ".
printf '%s\n' \
  '=== RUN   TestPinned' \
  '    pin_test.go:10: TestPinned in its own process (pid 7):' \
  '        child| === RUN   TestPinned' \
  '        child| --- SKIP: TestPinned (0.00s)' \
  '--- SKIP: TestPinned (0.01s)' >"$dir/child.log"
check "the child process's SKIP line is not counted and not a failure" "$dir/child.log" 0 1

printf '%s\n' '        child| --- SKIP: TestPinned (0.00s)' >"$dir/child-only.log"
check "a log with only child SKIP lines counts 0" "$dir/child-only.log" 0 0

: >"$dir/empty.log"
check "an empty log counts 0" "$dir/empty.log" 0 0

status=0
"$helper" "$dir/missing.log" >/dev/null 2>&1 || status=$?
if [ "$status" -ne 2 ]; then
  echo "FAIL a missing log exits $status, want 2"
  fail=1
else
  echo "ok   a missing log exits 2"
fi

exit "$fail"
