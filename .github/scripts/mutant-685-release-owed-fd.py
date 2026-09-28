#!/usr/bin/env python3
"""celeris#685 detector control: the MUTANT, applied in CI and never committed.

Makes fdOwed (engine/iouring/fd_lifetime.go) report false for every
connection, which takes the fd-lifetime rule off every path it guards at
once: the close paths close the descriptor at once again (no kept number,
no read-side shutdown), hijackConn no longer submits its cancels before
handing the socket over, and worker shutdown no longer ends the owed ops
before closing descriptors.

Against the mutated tree every run of the five trials that judge the rule
MUST detect the theft, as its result line reports it (the CI step reads
that line, not the --- line, so a failure for another reason does not
count): TestRecvTheft715ArmA and its hole twin TestRecvTheft715ArmAHole (a
recv still in the SQ ring at the close), TestRecvTheft685Linked and
TestRecvTheft685LinkedHole (a recv linked behind a SEND, not issued at the
close), each with hit=true reused=true stolen=true, and
TestRecvTheft685HijackMultishotCoop (a multishot recv owed at a Hijack on a
ring without DEFER_TASKRUN) with hijacker_read=false. If one does not, it
is not watching the path it claims to judge, and its green run on the tree
as committed proves nothing.

The mutant also takes the rule off worker shutdown (endOwedOpsAtShutdown
ends only the ops fdOwed reports), but no trial judges that half: it is
covered by construction, not by a detector.

The function is matched EXACTLY; any drift makes this script exit 2 instead
of silently mutating nothing.
"""
import sys

PATH = sys.argv[1] if len(sys.argv) > 1 else "engine/iouring/fd_lifetime.go"

BODY = (
    "func fdOwed(cs *connState) bool {\n"
    "\treturn cs != nil && fdOps(cs) > 0 && !cs.fixedFile\n"
    "}\n"
)

src = open(PATH).read()
if src.count(BODY) != 1:
    print(f"mutant-685: expected exactly one fdOwed body, found {src.count(BODY)}", file=sys.stderr)
    sys.exit(2)
mutated = src.replace(
    BODY,
    "func fdOwed(cs *connState) bool {\n"
    "\t// MUTANT celeris#685: the fd-lifetime rule is off everywhere\n"
    "\t_ = cs\n"
    "\treturn false\n"
    "}\n",
)
open(PATH, "w").write(mutated)
print(f"mutant-685: fdOwed always false ({PATH})")
