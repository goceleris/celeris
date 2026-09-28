#!/usr/bin/env python3
"""celeris#685 detector control: the MUTANT, applied in CI and never committed.

Makes fdOwed (engine/iouring/fd_lifetime.go) report false for every
connection, which takes the fd-lifetime rule off every path it guards at
once: the close paths close the descriptor at once again (no kept number,
no read-side shutdown), hijackConn no longer submits its cancels before
handing the socket over, and worker shutdown no longer ends the owed ops
before closing descriptors.

Against the mutated tree the three trials that judge the rule MUST fail in
every run: TestRecvTheft715ArmA (a recv still in the SQ ring at the close),
TestRecvTheft685Linked (a recv linked behind a SEND, not issued at the
close) and TestRecvTheft685HijackMultishotCoop (a multishot recv owed at a
Hijack on a ring without DEFER_TASKRUN). If one passes, it is not watching
the path it claims to judge, and its green run on the tree as committed
proves nothing.

The function is matched EXACTLY; any drift makes this script exit 2 instead
of silently mutating nothing.
"""
import sys

PATH = sys.argv[1] if len(sys.argv) > 1 else "engine/iouring/fd_lifetime.go"

BODY = (
    "func fdOwed(cs *connState) bool {\n"
    "\treturn cs != nil && cs.kernelInflight > 0 && !cs.fixedFile\n"
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
