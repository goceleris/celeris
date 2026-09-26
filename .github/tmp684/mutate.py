#!/usr/bin/env python3
"""celeris#684 lane F RULE 5 mutants (THROWAWAY branch tmp/684-proof only; copied there as
.github/tmp684/mutate.py and applied inside the CI job, never committed as product code).

  sendfile-hook   engine/epoll/loop.go: stop installing the sendfile hook on the H1 adapter
                  (the wire-up the e2e file exists to pin). LargeFile and HEAD must FAIL.
  sendfile-range  engine/epoll/loop.go: the hook ignores the caller's offset (range bodies
                  come from byte 0), and internal/conn/response.go: the 16 KiB threshold gate
                  is removed (small bodies go through sendfile). Range and SubThreshold must
                  FAIL; the two do not mask each other (the range body is above the threshold,
                  the 4 KiB body is served from offset 0 anyway).
  mut592          router.go: reopenSettled no longer clears the settled set (reverts the
                  celeris#592 re-opener, PR #603). Both engine halves' `settled` subtests of
                  TestAdaptiveSettledRouteRetime592 must FAIL wherever they run.
  mut593          engine/iouring/worker.go: snapshotH1Deadlines blocks on detachMu again
                  instead of TryLock (reverts celeris#593 / PR #604). Asks whether the io_uring
                  half of TestAdaptiveSettledRouteRetime592 catches it (measured on P0: it does
                  not -- the rig prints the pin as ANOMALY589 / IOURING592 but asserts only a
                  median ratio, so its verdict is PASS).

    mutate.py <name> [tree=.]
"""
import sys, pathlib

name = sys.argv[1]
tree = pathlib.Path(sys.argv[2] if len(sys.argv) > 2 else ".")


def edit(rel, old, new):
    f = tree / rel
    s = f.read_text()
    n = s.count(old)
    if n != 1:
        sys.exit("mutant %s: ANCHOR COUNT %d != 1 in %s: %r" % (name, n, rel, old[:80]))
    f.write_text(s.replace(old, new))
    print("mutant %s applied to %s" % (name, rel))


if name == "sendfile-hook":
    edit("engine/epoll/loop.go",
         "\t\t\tcs.h1State.SetSendFileFn(l.makeSendFileFn(cs))\n",
         "\t\t\t// MUTANT sendfile-hook: hook not installed\n")
elif name == "sendfile-range":
    edit("engine/epoll/loop.go",
         "\t\tst, err := newSendfileState(df, offset, length, header)\n",
         "\t\tst, err := newSendfileState(df, 0, length, header) // MUTANT sendfile-range: offset ignored\n")
    edit("internal/conn/response.go",
         "\tif length < sendfileThreshold {\n",
         "\tif length < 0 { // MUTANT sendfile-range: threshold gate removed\n")
elif name == "mut592":
    edit("router.go",
         "func (r *router) reopenSettled() {\n\tr.settled.Clear()\n}\n",
         "func (r *router) reopenSettled() {\n\t// MUTANT mut592: the settled set is never re-opened (celeris#592 reverted)\n}\n")
elif name == "mut593":
    edit("engine/iouring/worker.go",
         "\t\tif !mu.TryLock() {\n\t\t\treturn snap, false\n\t\t}\n\t\tdefer mu.Unlock()\n",
         "\t\tmu.Lock() // MUTANT mut593: blocking Lock, celeris#593 reverted\n\t\tdefer mu.Unlock()\n")
else:
    sys.exit("unknown mutant " + name)
