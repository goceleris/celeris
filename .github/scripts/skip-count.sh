#!/usr/bin/env bash
# skip-count.sh LOG: the number of `--- SKIP` lines of a `go test -v` log (celeris#890).
#
# Prints the count of lines that start (after optional indentation) with
# `--- SKIP: `, subtests included: the lines go test prints for a test that
# skipped. The tallies of ci.yml use it in place of a bare
# `grep -c '^[[:space:]]*--- SKIP: '`, which fails open: a test that writes to
# stdout without a trailing newline joins the next `--- SKIP: ` line to its
# own, the joined line does not start with `--- SKIP: `, and the grep does not
# count it (the PASS-by-name tallies fail closed in the same case: the PASS
# line they look for is missing).
#
# So the script also counts the `--- SKIP: ` lines that do not start with it
# and exits 1, listing them on stderr, when there are any. The one exception is
# a line that internal/platform/pintest's RunInOwnProcess logs: the output of a
# child process, every line prefixed "child| ", which holds that process's own
# `--- SKIP: ` lines and is not a result of this one. The count goes to stdout
# only, so `skipped=$(skip-count.sh LOG)` keeps stderr for the listing, and a
# step that runs with `-e` (the default `bash` shell of a workflow step) stops
# at the failing assignment.
#
# Exit status: 0 counted, 1 a `--- SKIP: ` line the anchored count would miss,
# 2 usage or an unreadable log.
set -euo pipefail

[ "$#" -eq 1 ] || { echo "usage: skip-count.sh LOG" >&2; exit 2; }
log=$1
[ -r "$log" ] || { echo "skip-count: cannot read $log" >&2; exit 2; }

# grep exits 1 when nothing matches; that is a count of 0 here, not a failure.
# -a: a log with a NUL byte (a test that wrote one) is text here; without it GNU grep prints
# "binary file matches" to stderr in place of the line and the count falls to 0.
anchored=$(grep -acE '^[[:space:]]*--- SKIP: ' "$log" || true)
# Every line holding `--- SKIP: ` anywhere, except the child process's own.
loose=$(grep -aF -e '--- SKIP: ' "$log" | tr -d '\000' | grep -vE '^[[:space:]]*child\| ' || true)
unanchored=0
if [ -n "$loose" ]; then
  unanchored=$(printf '%s\n' "$loose" | wc -l | tr -d '[:space:]')
fi

if [ "$unanchored" -gt "$anchored" ]; then
  echo "skip-count: $log: $unanchored lines hold '--- SKIP: ' but only $anchored start with it; the rest are SKIP results a tally would not count" >&2
  echo "(a test wrote to stdout without a newline before go test printed its result line?):" >&2
  printf '%s\n' "$loose" | grep -vE '^[[:space:]]*--- SKIP: ' | cut -c1-300 >&2 || true
  exit 1
fi
echo "$anchored"
