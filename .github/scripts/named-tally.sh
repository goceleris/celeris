#!/usr/bin/env bash
# named-tally.sh LABEL LOG NAME...: the by-name tally of a `go test -v` log (celeris#890).
#
# Every NAME is the full name of a top-level test and must, in LOG, run once
# (`=== RUN   NAME`), PASS once (`--- PASS: NAME (`) and not skip, at the top
# level or in a subtest (`--- SKIP: NAME ` or `--- SKIP: NAME/`), and not fail.
# A test that was renamed, removed or never started has no RUN line, so it is
# absent, not a pass; a test that skipped has a RUN line and no PASS line. A
# line a test's own stdout joined to the result line (`x--- PASS: ...`) does not
# match either, so it fails closed.
#
# The line printed (stdout) reads
#   LABEL: want N, ran N, passed N, SKIP lines K, FAIL lines F
# with the sums over the names, and every name that is not exactly (1, 1, 0, 0)
# is listed under it on stdout as
#   LABEL: NAME ran R, passed P, SKIP lines S, FAIL lines F
# The names are matched as written and may not hold a regular expression's
# characters: a name outside [A-Za-z0-9_] or listed twice is a usage error.
#
# Exit status: 0 every name ran once and passed, 1 some did not, 2 usage or an
# unreadable log.
set -euo pipefail

[ "$#" -ge 3 ] || { echo "usage: named-tally.sh LABEL LOG NAME..." >&2; exit 2; }
label=$1
log=$2
shift 2
[ -r "$log" ] || { echo "named-tally: cannot read $log" >&2; exit 2; }

want=0 ran=0 passed=0 skipped=0 failed=0 bad=0
seen=" "
detail=""
for name in "$@"; do
  case "$name" in
    '' | *[!A-Za-z0-9_]*) echo "named-tally: '$name' is not a test name ([A-Za-z0-9_]+)" >&2; exit 2 ;;
  esac
  case "$seen" in *" $name "*) echo "named-tally: $name is listed twice" >&2; exit 2 ;; esac
  seen="$seen$name "
  # grep -a: a NUL byte in a log must not turn a match into "binary file matches".
  # grep exits 1 on no match; that is a count of 0 here.
  r=$(grep -acE "^=== RUN   ${name}\$" "$log" || true)
  p=$(grep -acE "^--- PASS: ${name} \(" "$log" || true)
  s=$(grep -acE "^[[:space:]]*--- SKIP: ${name}[ /]" "$log" || true)
  f=$(grep -acE "^[[:space:]]*--- FAIL: ${name}[ /]" "$log" || true)
  want=$((want + 1))
  ran=$((ran + r))
  passed=$((passed + p))
  skipped=$((skipped + s))
  failed=$((failed + f))
  if [ "$r" -ne 1 ] || [ "$p" -ne 1 ] || [ "$s" -ne 0 ] || [ "$f" -ne 0 ]; then
    bad=$((bad + 1))
    detail="${detail}${label}: ${name} ran ${r}, passed ${p}, SKIP lines ${s}, FAIL lines ${f}"$'\n'
  fi
done

echo "${label}: want ${want}, ran ${ran}, passed ${passed}, SKIP lines ${skipped}, FAIL lines ${failed}"
if [ "$bad" -ne 0 ]; then
  printf '%s' "$detail"
  exit 1
fi
