#!/usr/bin/env bash
# base-tier-tally.sh LABEL LOG [NAME...]: the tally of a `go test -v` log run
# with CELERIS_MAX_IOURING_TIER=base (celeris#890 item 87).
#
# LABEL is "iouring" (the log of ./internal/engine/iouring) or "root" (the log of
# the root package and ./test/server). Three checks, each failing the step:
#
#   1. No test skips except the ones the allow-list of that label names, each
#      with the reason it may skip at the base tier. A SKIP line is matched by
#      the test's full name, a subtest included (`--- SKIP: Test/sub (`), and
#      an entry covers its own subtests. A skip is not coverage; a test that
#      needs the high tier says so here.
#   2. Every NAME is a top-level test that ran once and PASSED, with no SKIP or
#      FAIL line (named-tally.sh).
#   3. For "iouring": the cap took effect. TestReportTier844 printed the capped
#      probe once as io_uring_tier=base with the variable set to "base", and no
#      engine arm of the celeris#844 tests reported tier=high. A value the
#      probe does not know counts as "none", which would skip every io_uring
#      test and leave check 1 as the only witness; this one names the cause.
#
#   4. At least `min-pass` top-level tests passed (the data file of the label).
#
# Prints one summary line on stdout. Exit status 0 all hold, 1 some do not,
# 2 usage or an unreadable log.
set -euo pipefail

[ "$#" -ge 2 ] || { echo "usage: base-tier-tally.sh LABEL LOG [NAME...]" >&2; exit 2; }
label=$1
log=$2
shift 2
[ -r "$log" ] || { echo "base-tier-tally: cannot read $log" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)

# Allowed skips: .github/base-tier-skips-LABEL.txt, one entry per line, then the reason
# (blank lines and # lines are ignored; BASE_TIER_SKIPS_DIR points elsewhere for the
# self-test). An entry is
#   NAME            a test or subtest by its full name; it covers its own subtests, or
#   pattern:REGEX   a skipped test or subtest whose full name matches REGEX, and only
#                   when the test itself logged the probe's skip message while it was the
#                   current test ("file_test.go:N: io_uring tier=base kernel=...: no
#                   usable io_uring"). The engine's own startup line ("INFO io_uring engine
#                   selected tier=base ...") does not count: an arm that started an
#                   engine and then skipped for another reason must still fail.
#   min-pass N      the top-level tests that must PASS at least, so that a -run pattern
#                   that stops matching (a rename) or a package that stops compiling its
#                   tests in cannot shrink the run unseen; the figure is measured.
case "$label" in
  iouring | root) ;;
  *) echo "base-tier-tally: label '$label' is neither iouring nor root" >&2; exit 2 ;;
esac
list_file=${BASE_TIER_SKIPS_DIR:-$here/..}/base-tier-skips-$label.txt
[ -r "$list_file" ] || { echo "base-tier-tally: cannot read $list_file" >&2; exit 2; }
# Names are literal (compared as strings below, not as regular expressions).
names=$(awk 'NF && $1 !~ /^#/ && $1 != "min-pass" && $1 !~ /^pattern:/ { print $1 }' "$list_file" | paste -sd'|' -)
patterns=$(awk 'NF && $1 ~ /^pattern:/ { sub(/^pattern:/, "", $1); print $1 }' "$list_file" | paste -sd'|' -)
minpass=$(awk '$1 == "min-pass" { print $2 }' "$list_file")

fail=0
skipped=$("$here"/skip-count.sh "$log") || fail=1
# Each SKIP line is judged by the name of the test: allowed by an exact entry (or as a
# subtest of one), or by a pattern while the test logged the probe's tier=base skip message.
unlisted=$(awk -v names="$names" -v pats="$patterns" 'BEGIN { nn = (names == "") ? 0 : split(names, nm, "|") }
  # Parallel subtests interleave their output; go test names the owner of the lines that
  # follow with RUN, and with NAME or CONT when it switches back to a test.
  /^=== (RUN|NAME|CONT) / { cur = $3; next }
  /^[[:space:]]+[A-Za-z0-9_.]+\.go:[0-9]+: .*io_uring tier=base kernel=.*: no usable io_uring/ && cur != "" { base[cur] = 1 }
  /^[[:space:]]*--- SKIP: / {
    n = $3
    ok = 0
    for (i = 1; i <= nn; i++) if (n == nm[i] || index(n, nm[i] "/") == 1) ok = 1
    if (!ok && pats != "" && n ~ pats && base[n]) ok = 1
    if (!ok) print
  }
' "$log")
n_unlisted=0
[ -n "$unlisted" ] && n_unlisted=$(printf '%s\n' "$unlisted" | wc -l | tr -d '[:space:]')

tiers=
if [ "$label" = iouring ]; then
  probe=$(grep -acE 'celeris844 PROBE .* io_uring_tier=base CELERIS_MAX_IOURING_TIER="base"$' "$log" || true)
  high=$(grep -acE 'celeris844 RESULT engine=io_uring tier=high ' "$log" || true)
  base=$(grep -acE 'celeris844 RESULT engine=io_uring tier=base ' "$log" || true)
  tiers=", capped probe lines $probe (want 1), engine arms at tier=base $base (want > 0) and tier=high $high (want 0)"
  if [ "$probe" -ne 1 ] || [ "$high" -ne 0 ] || [ "$base" -eq 0 ]; then
    fail=1
    echo "base-tier-tally: the cap did not take effect: CELERIS_MAX_IOURING_TIER=base is not the tier the io_uring arms ran on" >&2
  fi
fi

passed=$(grep -acE '^--- PASS: Test' "$log" || true)
if [ -n "$minpass" ] && [ "$passed" -lt "$minpass" ]; then
  fail=1
  echo "base-tier-tally: $passed top-level tests passed, fewer than the $minpass of $list_file: did the run shrink (a renamed test, a -run pattern that no longer matches)?" >&2
fi
echo "base-tier-tally $label: top-level PASS $passed (min ${minpass:-none}), SKIP lines $skipped, not in the allow-list $n_unlisted$tiers"
if [ "$n_unlisted" -ne 0 ]; then
  fail=1
  printf '%s\n' "$unlisted" >&2
  echo "base-tier-tally: tests skipped at the base tier that the allow-list of '$label' does not name;" >&2
  echo "add a name only with the reason it must skip at this tier (a skip is not coverage)" >&2
fi
if [ "$#" -gt 0 ]; then
  "$here"/named-tally.sh "base tier $label" "$log" "$@" || fail=1
fi
exit "$fail"
