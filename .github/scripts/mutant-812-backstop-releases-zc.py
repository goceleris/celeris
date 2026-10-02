#!/usr/bin/env python3
"""celeris#812 detector control: the MUTANT, applied in CI and never committed.

Disables the hold in drainPendingRelease (engine/iouring/worker.go): past the
release backstop, an entry that still owes a SEND_ZC is released like any
other, to connStatePool with its send buffer (a detached one to the GC), as
before the fix. Everything else of the fix stays: the accounting, the
counters and the shutdown retention.

Against the mutated tree every run of every case of
TestBackstopHoldsASendBufferAZCNotificationStillReads MUST detect the release:
its result line reports held_past_backstop=false and corrupt_bytes above 0
(the peer read the array's next owner's bytes), and the case FAILs. If one
does not, the test is not watching what it claims to on this runner (a kernel
that copied the send's pages before the release would pass it with or without
the fix), and its green run on the tree as committed proves nothing.

The statement is matched EXACTLY; any drift makes this script exit 2 instead
of silently mutating nothing.
"""
import sys

PATH = sys.argv[1] if len(sys.argv) > 1 else "engine/iouring/worker.go"

HOLD = "\t\t\tif w.closedZCOwed(cs) {\n\t\t\t\tw.holdZCPastBackstop(entry)\n"

src = open(PATH).read()
if src.count(HOLD) != 1:
    print(f"mutant-812: expected exactly one backstop hold, found {src.count(HOLD)}", file=sys.stderr)
    sys.exit(2)
mutated = src.replace(
    HOLD,
    "\t\t\t// MUTANT celeris#812: the backstop releases a SEND_ZC's send buffer\n"
    "\t\t\tif false && w.closedZCOwed(cs) {\n\t\t\t\tw.holdZCPastBackstop(entry)\n",
)
open(PATH, "w").write(mutated)
print(f"mutant-812: backstop hold disabled ({PATH})")
