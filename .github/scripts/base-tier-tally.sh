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
# Prints one summary line on stdout. Exit status 0 all hold, 1 some do not,
# 2 usage or an unreadable log.
set -euo pipefail

[ "$#" -ge 2 ] || { echo "usage: base-tier-tally.sh LABEL LOG [NAME...]" >&2; exit 2; }
label=$1
log=$2
shift 2
[ -r "$log" ] || { echo "base-tier-tally: cannot read $log" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)

# Allowed skips: an alternation of full test names, one entry per line with the
# reason after the name. Measured on the runs cited in the pull request.
allowed_iouring() {
  cat <<'LIST'
LIST
}
allowed_root() {
  cat <<'LIST'
LIST
}

case "$label" in
  iouring) list=$(allowed_iouring) ;;
  root) list=$(allowed_root) ;;
  *) echo "base-tier-tally: label '$label' is neither iouring nor root" >&2; exit 2 ;;
esac
names=$(printf '%s\n' "$list" | awk 'NF { print $1 }' | paste -sd'|' -)

fail=0
skipped=$("$here"/skip-count.sh "$log") || fail=1
if [ -n "$names" ]; then
  unlisted=$(grep -aE '^[[:space:]]*--- SKIP: ' "$log" | grep -avE "^[[:space:]]*--- SKIP: (${names})( |/)" || true)
else
  unlisted=$(grep -aE '^[[:space:]]*--- SKIP: ' "$log" || true)
fi
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

echo "base-tier-tally $label: SKIP lines $skipped, not in the allow-list $n_unlisted$tiers"
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
