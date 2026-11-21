#!/usr/bin/env bash
# host-skip-reason.sh LOG: the tests of internal/engine/iouring that skip by design on a host
# that cannot make the condition they test may skip only FOR THAT REASON. The allow-lists name
# the tests; this checks the reason: the log lines a test printed after its `=== RUN` and before
# its `--- SKIP` must carry the reason text below. A skip for any other reason of the same test
# (no io_uring at all, a failed setrlimit, no loopback listener, no ring created at all) is not
# a host-dependent skip and fails here.
#
# Prints, on stdout, "host-dependent skips: N" (N = SKIP lines of the named tests that carry
# their reason) and lists each. Exit status 0 all such skips carry their reason, 1 some do not,
# 2 usage or an unreadable log. Add a test here only with the reason it may skip on a host.
set -euo pipefail

[ "$#" -eq 1 ] || { echo "usage: host-skip-reason.sh LOG" >&2; exit 2; }
log=$1
[ -r "$log" ] || { echo "host-skip-reason: cannot read $log" >&2; exit 2; }

# NAME<TAB>reason text the test's own skip message contains (celeris#756 CI fix).
rules=$(printf '%s\t%s\n' \
  TestStartFDLEngineRidesOutRingENOMEM 'could not fill the memlock budget (512 rings held)' \
  TestDialWatchDescribesTheSocketsOfAPort1229 'did not leave the dial to 10.255.255.1')

RULES="$rules" awk '
  BEGIN {
    n = split(ENVIRON["RULES"], r, "\n")
    for (i = 1; i <= n; i++) { split(r[i], kv, "\t"); want[kv[1]] = kv[2] }
  }
  # Parallel tests interleave their output: `go test -v` names the owner of the lines that
  # follow with RUN, NAME or CONT.
  /^=== (RUN|NAME|CONT) / { cur = $3; if ($2 == "RUN") seen[cur] = ""; next }
  /^[[:space:]]*--- SKIP: / {
    name = $3
    if (!(name in want)) next
    if (index(seen[name], want[name]) > 0) { ok++; print "ok: " $0 }
    else { bad++; print "NOT the host reason: " $0 }
    next
  }
  { if (cur in want) seen[cur] = seen[cur] "\n" $0 }
  END {
    printf "host-dependent skips: %d\n", ok
    if (bad > 0) {
      printf "host-skip-reason: %d skip(s) of a host-dependent test without its reason (the test skipped for another cause)\n", bad > "/dev/stderr"
      exit 1
    }
  }
' "$log"
