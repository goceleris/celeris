#!/usr/bin/env python3
"""celeris#587 detector control: the MUTANT, applied in CI and never committed.

Releases cs.detachMu at the top of handleSend's CQE_F_MORE branch
(engine/iouring/worker.go), i.e. the SEND_ZC first completion then writes
cs.sending / cs.zcNotifPending / cs.zcSentBytes with no lock, while the
inline-egress guard on the dispatch goroutine reads them under the lock.
handleSend takes the lock once, at its top (celeris#750), and the branch
defers its release; the mutant releases it at once instead.

TestSendZCWindowGuardUnderRace, run under -race against the mutated tree,
MUST fail with a "WARNING: DATA RACE" report. If it does not, the race
detector is not watching the path the test claims to judge and the test's
green run proves nothing.

The block is matched EXACTLY and must occur exactly once inside the
F_MORE branch; any drift in the source makes this script exit 2 instead of
silently mutating nothing (a mutant that is not applied would "survive"
for the wrong reason).
"""
import sys

PATH = sys.argv[1] if len(sys.argv) > 1 else "engine/iouring/worker.go"

ANCHOR = "\tif cqeHasMore(c.Flags) {\n"
LOCK = (
    "\t\tif mu != nil {\n"
    "\t\t\tdefer mu.Unlock()\n"
    "\t\t}\n"
)
MUTANT = (
    "\t\t// MUTANT celeris#587: detachMu released before the F_MORE writes\n"
    "\t\tif mu != nil {\n"
    "\t\t\tmu.Unlock()\n"
    "\t\t}\n"
)

src = open(PATH).read()
if src.count(ANCHOR) != 1:
    print(f"mutant-587: expected exactly one F_MORE branch anchor, found {src.count(ANCHOR)}", file=sys.stderr)
    sys.exit(2)
start = src.index(ANCHOR) + len(ANCHOR)
# The deferred release must be the first statement block of the branch
# (after its comment lines), within a short distance of the anchor.
window = src[start:start + 600]
if window.count(LOCK) != 1:
    print("mutant-587: the deferred detachMu release was not found at the top of the F_MORE branch", file=sys.stderr)
    sys.exit(2)
i = start + window.index(LOCK)
mutated = src[:i] + MUTANT + src[i + len(LOCK):]
open(PATH, "w").write(mutated)
print(f"mutant-587: released detachMu before the writes of handleSend's CQE_F_MORE branch ({PATH})")
