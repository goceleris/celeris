#!/usr/bin/env bash
# timeout-headroom.sh LOG TIMEOUT_SECONDS: how close a `go test` step came to its -timeout (celeris#890).
#
# `go test -timeout=N` applies to each test binary, so the figure that matters is the
# longest `ok <package> <seconds>s` line of the step's log (the line carries the binary's
# run time, without the build). The script prints that figure as a share of the timeout and,
# when it is above HEADROOM_WARN_PCT per cent (default 75), emits a `::warning::` annotation
# and, if GITHUB_STEP_SUMMARY is set, a line in the job summary. It does NOT fail the step
# on a high figure: a step 5 per cent slower on a busy runner would turn a required check
# red for every unrelated pull request, which is the harm the headroom is there to prevent.
# The warning is how the next regression becomes visible before it reaches the wall; the
# timeout panic is still the only hard failure.
#
# The ci.yml and test-coverage.yml steps pass the same variable to `go test -timeout=` and
# to this script, so the two cannot drift apart. Run it after the step's own tallies, on the
# `tee`d log of a run that passed.
#
# Exit status: 0 reported, 1 the log has no `ok` line (fail closed: a step whose log shows no
# passing package has nothing to measure, and a silent 0 would hide a log that stopped
# being captured), 2 usage or an unreadable log.
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: timeout-headroom.sh LOG TIMEOUT_SECONDS" >&2; exit 2; }
log=$1
timeout_s=$2
[ -r "$log" ] || { echo "timeout-headroom: cannot read $log" >&2; exit 2; }
case $timeout_s in '' | *[!0-9]* | 0) echo "timeout-headroom: TIMEOUT_SECONDS must be a positive integer, got '$timeout_s'" >&2; exit 2 ;; esac
warn_pct=${HEADROOM_WARN_PCT:-75}
case $warn_pct in '' | *[!0-9]*) echo "timeout-headroom: HEADROOM_WARN_PCT must be an integer, got '$warn_pct'" >&2; exit 2 ;; esac

# `ok<spaces><package><spaces><seconds>s`, optionally followed by a coverage figure.
# -a: a log with a NUL byte is text here (as in skip-count.sh).
slowest=$(grep -aE '^ok[[:space:]]+[^[:space:]]+[[:space:]]+[0-9]+(\.[0-9]+)?s([[:space:]]|$)' "$log" |
  awk '{ t = $3; sub(/s$/, "", t); if (t + 0 >= max) { max = t + 0; pkg = $2 } } END { if (pkg != "") printf "%s %s\n", pkg, max }' || true)
if [ -z "$slowest" ]; then
  echo "timeout-headroom: no 'ok <package> <seconds>s' line in $log -- did the step's output stop being captured?" >&2
  exit 1
fi
pkg=${slowest% *}
secs=${slowest#* }

msg=$(awk -v pkg="$pkg" -v t="$secs" -v lim="$timeout_s" 'BEGIN {
  printf "%s took %.1f s of its %d s timeout: %.0f%% of the timeout, %.0f%% headroom over the run", pkg, t, lim, 100 * t / lim, 100 * (lim - t) / t
}')
echo "timeout headroom: $msg"
# Compared as t * 100 > timeout * pct, so no rounding decides it.
over=$(awk -v t="$secs" -v lim="$timeout_s" -v pct="$warn_pct" 'BEGIN { print (t * 100 > lim * pct) ? 1 : 0 }')
if [ "$over" -eq 1 ]; then
  echo "::warning title=go test timeout headroom::$msg; above ${warn_pct}% of the timeout. The next slow run or added test may end in a timeout panic: split the package or raise the timeout (celeris#890)."
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- :warning: $msg (above ${warn_pct}% of the timeout)" >>"$GITHUB_STEP_SUMMARY"
  fi
fi
