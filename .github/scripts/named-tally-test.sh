#!/usr/bin/env bash
# Self-test of named-tally.sh and of the ci.yml step that calls it (celeris#890
# items 18, 19, 61). Run from the repository root; the Lint job runs it, so a
# change to the helper that makes it miscount, a name in the step that matches
# no test, or a step that stops failing on a missing or skipped test is a red
# Lint and not a tally that quietly counts less.
#
# The fixtures are the two logs the step reads, cut from the real `go test -v`
# output of CI run 39404206686 (the Unit job, d80d94a): testdata/named-tally-
# iouring.log is /tmp/iouring-pkg.log (the iouring step, 415.129 s) and
# testdata/named-tally-root.log is /tmp/root.log (the root step), each
# reduced to the RUN and result lines of the tests the step counts, the three
# allowed SKIP lines of the iouring step, the `child| ` lines of the pintest
# tests and the `ok` lines (the full logs are 2.5 and 24 thousand lines).
#
# The step's own `run:` block is read out of ci.yml and run on the fixtures with
# /tmp rewritten to a temporary directory, so the name lists the test checks are
# the ones CI runs. Every name in it must also be a test function of this
# repository.
#
#   NAMED_TALLY=path  the helper to test (a negative control points it at a
#                     mutated copy and must see this test fail)
#   CI_YML=path       the workflow to read the step from
set -euo pipefail

helper=${NAMED_TALLY:-.github/scripts/named-tally.sh}
case "$helper" in /*) ;; *) helper=$PWD/$helper ;; esac
ci=${CI_YML:-.github/workflows/ci.yml}
iou_fixture=.github/scripts/testdata/named-tally-iouring.log
root_fixture=.github/scripts/testdata/named-tally-root.log
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
fail=0

# The step's script, with the logs under $dir and the helper as given.
awk '
  /^      - name: celeris#890 by-name tally/ { s = 1; next }
  s == 1 && /^        run: \|$/ { s = 2; next }
  s == 2 { if ($0 ~ /^          / || $0 ~ /^$/) { sub(/^          /, ""); print } else exit }
' "$ci" | sed -e "s#/tmp/#$dir/#g" -e "s#\"\$GITHUB_WORKSPACE\"/.github/scripts/named-tally.sh#$helper#" >"$dir/step.sh"
if ! grep -q 'tally=' "$dir/step.sh" || ! grep -q "$helper" "$dir/step.sh"; then
  echo "FAIL could not read the step 'celeris#890 by-name tally' out of $ci"
  exit 1
fi

# step NAME IOURING_LOG ROOT_LOG WANT_STATUS [WANT_OUTPUT_SUBSTRING...]
step() {
  local name=$1 iou=$2 root=$3 want=$4 out status=0
  shift 4
  [ "$iou" = - ] || cp "$iou" "$dir/iouring-pkg.log"
  [ "$root" = - ] || cp "$root" "$dir/root.log"
  out=$(GITHUB_WORKSPACE=$PWD bash -eo pipefail "$dir/step.sh" 2>&1) || status=$?
  if { [ "$want" = 0 ] && [ "$status" -ne 0 ]; } || { [ "$want" != 0 ] && [ "$status" -eq 0 ]; }; then
    echo "FAIL $name: step exit $status (want ${want/#[^0]/non-zero})"
    printf '%s\n' "$out" | sed 's/^/     /'
    fail=1
    return
  fi
  local sub
  for sub in "$@"; do
    if ! grep -qF -e "$sub" <<<"$out"; then
      echo "FAIL $name: output lacks '$sub':"
      printf '%s\n' "$out" | sed 's/^/     /'
      fail=1
      return
    fi
  done
  echo "ok   $name"
}

cp "$iou_fixture" "$dir/iou.base"
cp "$root_fixture" "$dir/root.base"

step "the complete logs pass, each group tallied" "$dir/iou.base" "$dir/root.base" 0 \
  ': want 6, ran 6, passed 6, SKIP lines 0, FAIL lines 0' \
  ': want 10, ran 10, passed 10, SKIP lines 0, FAIL lines 0' \
  'twins (celeris#713): want 2, ran 2, passed 2' \
  '(celeris#909): want 3, ran 3, passed 3' \
  '(celeris#919): want 1, ran 1, passed 1'

# A test renamed or removed: no RUN line, no PASS line.
sed 's/TestParkedWorkerSendsFINForAReadTimeoutClose/TestParkedWorkerSendsFINForAReadTimeoutCloseRenamed/' "$dir/iou.base" >"$dir/iou.renamed"
step "a renamed park_close_fin test fails and is named" "$dir/iou.renamed" "$dir/root.base" 1 \
  ': want 6, ran 5, passed 5' \
  'TestParkedWorkerSendsFINForAReadTimeoutClose ran 0, passed 0'
grep -v 'TestAdoptOntoADrainingWorkerIsNotTimedOut' "$dir/iou.base" >"$dir/iou.removed"
step "a removed park_stale_clock test fails and is named" "$dir/iou.removed" "$dir/root.base" 1 \
  ': want 10, ran 9, passed 9' \
  'TestAdoptOntoADrainingWorkerIsNotTimedOut ran 0, passed 0'
grep -v 'TestAcceptAfterALongParkIsNotTimedOutEpoll' "$dir/root.base" >"$dir/root.notwin"
step "a removed epoll twin fails" "$dir/iou.base" "$dir/root.notwin" 1 \
  'TestAcceptAfterALongParkIsNotTimedOutEpoll ran 0, passed 0'
grep -v 'TestInlineHandlerChildKeepsTheProcessMask919' "$dir/root.base" >"$dir/root.no919"
step "a removed 919 test fails" "$dir/iou.base" "$dir/root.no919" 1 \
  'TestInlineHandlerChildKeepsTheProcessMask919 ran 0, passed 0'

# A test that skipped: a RUN line, a SKIP line, no PASS line.
sed 's/^--- PASS: TestAdoptAfterAShortParkIsNotTimedOut /--- SKIP: TestAdoptAfterAShortParkIsNotTimedOut /' "$dir/iou.base" >"$dir/iou.skip"
step "a skipped park_stale_clock test fails" "$dir/iou.skip" "$dir/root.base" 1 \
  'TestAdoptAfterAShortParkIsNotTimedOut ran 1, passed 0, SKIP lines 1'
sed 's/^--- PASS: TestUnpinnedLoopsStayUnpinned909 /--- SKIP: TestUnpinnedLoopsStayUnpinned909 /' "$dir/root.base" >"$dir/root.skip"
step "a skipped 909 test fails (the root step would forgive it after a re-run)" "$dir/iou.base" "$dir/root.skip" 1 \
  'TestUnpinnedLoopsStayUnpinned909 ran 1, passed 0, SKIP lines 1'
{ cat "$dir/root.base"; printf '    --- SKIP: TestLoopsPinInsideTheAllowedSet909/arm (0.00s)\n'; } >"$dir/root.subskip"
step "a skipped subtest of a counted test fails even with the parent's PASS" "$dir/iou.base" "$dir/root.subskip" 1 \
  'TestLoopsPinInsideTheAllowedSet909 ran 1, passed 1, SKIP lines 1'

# A test that failed or ran twice. A subtest's FAIL line stands beside the parent's
# PASS here to show the FAIL count is a check of its own; go test marks the parent
# failed too, so this does not happen in a real log.
{ cat "$dir/root.base"; printf '    --- FAIL: TestLoopsPinInsideTheAllowedSet909/arm (0.00s)\n'; } >"$dir/root.subfail"
step "a failed subtest of a counted test fails" "$dir/iou.base" "$dir/root.subfail" 1 \
  'TestLoopsPinInsideTheAllowedSet909 ran 1, passed 1, SKIP lines 0, FAIL lines 1'
sed 's/^--- PASS: TestInlineHandlerChildKeepsTheProcessMask919 /--- FAIL: TestInlineHandlerChildKeepsTheProcessMask919 /' "$dir/root.base" >"$dir/root.fail"
step "a failed 919 test fails" "$dir/iou.base" "$dir/root.fail" 1 \
  'TestInlineHandlerChildKeepsTheProcessMask919 ran 1, passed 0, SKIP lines 0, FAIL lines 1'
{ cat "$dir/iou.base"; printf '=== RUN   TestAdoptionIsStampedWithTheTimeItWasAdopted\n--- PASS: TestAdoptionIsStampedWithTheTimeItWasAdopted (0.00s)\n'; } >"$dir/iou.twice"
step "a test that ran twice fails (ran 2)" "$dir/iou.twice" "$dir/root.base" 1 \
  'TestAdoptionIsStampedWithTheTimeItWasAdopted ran 2, passed 2'

# Output a test wrote without a newline joins the next result line to its own:
# the PASS is then not at the start of its line and does not count.
sed 's/^--- PASS: TestAcceptAfterALongParkIsNotTimedOut (/x--- PASS: TestAcceptAfterALongParkIsNotTimedOut (/' "$dir/iou.base" >"$dir/iou.joined"
step "a joined PASS line fails closed" "$dir/iou.joined" "$dir/root.base" 1 \
  'TestAcceptAfterALongParkIsNotTimedOut ran 1, passed 0'

# The child process's lines (internal/platform/pintest prefixes them "child| ") are not
# this process's results: with the parent's own PASS gone the child's must not stand in.
grep -v '^--- PASS: TestFailedPinIsLoggedOnceAndNotPretended909 ' "$dir/root.base" >"$dir/root.nochild"
if ! grep -q 'child| --- PASS: TestFailedPinIsLoggedOnceAndNotPretended909' "$dir/root.nochild"; then
  echo "FAIL fixture: no child| PASS line of the failed-pin test to test with"
  fail=1
fi
step "the child process's PASS line does not stand in for the parent's" "$dir/iou.base" "$dir/root.nochild" 1 \
  'TestFailedPinIsLoggedOnceAndNotPretended909 ran 1, passed 0'

# A NUL byte in a log: without grep -a GNU grep calls the file binary and prints
# "binary file matches" in place of the lines it counts.
{ head -n 5 "$dir/iou.base"; printf '\000\n'; tail -n +6 "$dir/iou.base"; } >"$dir/iou.nul"
step "a NUL byte in a log does not hide the results" "$dir/iou.nul" "$dir/root.base" 0 ': want 10, ran 10, passed 10'
{ head -n 5 "$dir/iou.renamed"; printf '\000\n'; tail -n +6 "$dir/iou.renamed"; } >"$dir/iou.nulren"
step "a renamed test is still found after a NUL byte" "$dir/iou.nulren" "$dir/root.base" 1 'ran 0, passed 0'

# A step that did not write its log is a failure, not an empty tally.
rm -f "$dir/iouring-pkg.log"
step "a missing log fails" - "$dir/root.base" 1 'cannot read'
cp "$dir/iou.base" "$dir/iouring-pkg.log"

# The helper's own contract.
status=0
"$helper" 'x' "$dir/iou.base" TestAcceptAfterALongParkIsNotTimedOut >"$dir/out" 2>&1 || status=$?
if [ "$status" -eq 0 ] && grep -qF 'x: want 1, ran 1, passed 1, SKIP lines 0, FAIL lines 0' "$dir/out"; then
  echo "ok   a name is counted alone"
else
  echo "FAIL a single name: exit $status"
  cat "$dir/out"
  fail=1
fi
status=0
"$helper" 'x' "$dir/root.base" TestAcceptAfterALongParkIsNotTimedOut >"$dir/out" 2>&1 || status=$?
if [ "$status" -ne 1 ] || ! grep -qF 'TestAcceptAfterALongParkIsNotTimedOut ran 0, passed 0' "$dir/out"; then
  echo "FAIL the epoll twin's lines stand in for the io_uring test of the same name's prefix: exit $status"
  cat "$dir/out"
  fail=1
else
  echo "ok   a name is not matched as the prefix of a longer one"
fi
for bad in 'Test.*' 'Test A' 'Test/x' ''; do
  status=0
  "$helper" x "$dir/iou.base" "$bad" >/dev/null 2>&1 || status=$?
  if [ "$status" -ne 2 ]; then echo "FAIL the name '$bad' exits $status, want 2"; fail=1; else echo "ok   the name '$bad' is refused (2)"; fi
done
status=0
"$helper" x "$dir/iou.base" TestAcceptAfterALongParkIsNotTimedOut TestAcceptAfterALongParkIsNotTimedOut >/dev/null 2>&1 || status=$?
if [ "$status" -ne 2 ]; then echo "FAIL a name listed twice exits $status, want 2"; fail=1; else echo "ok   a name listed twice is refused (2)"; fi
status=0
"$helper" x "$dir/missing.log" TestA >/dev/null 2>&1 || status=$?
if [ "$status" -ne 2 ]; then echo "FAIL a missing log exits $status, want 2"; fail=1; else echo "ok   a missing log exits 2"; fi
status=0
"$helper" x "$dir/iou.base" >/dev/null 2>&1 || status=$?
if [ "$status" -ne 2 ]; then echo "FAIL no names exits $status, want 2"; fail=1; else echo "ok   no names exits 2"; fi

# Every name the step counts is a test function of this repository, so a rename of
# one is a red Lint at once, before the Unit job's tally says it.
names=$(grep -oE '\bTest[A-Za-z0-9_]+' "$dir/step.sh" | sort -u)
count=0
for n in $names; do
  count=$((count + 1))
  if ! git grep -qE "^func ${n}\(" -- '*_test.go'; then
    echo "FAIL the step counts $n, which is no test function of this repository"
    fail=1
  fi
done
if [ "$count" -ne 22 ]; then
  echo "FAIL the step names $count distinct tests, want 22 (6 + 10 + 2 + 3 + 1)"
  fail=1
else
  echo "ok   the step's $count names are test functions"
fi

exit "$fail"
