//go:build linux

package iouring

import "sync/atomic"

// asyncFeedHook is a test seam (celeris#884 items 82, 83, 84): nil in
// production, it runs on the worker's thread each time handleRecv's async feed
// has handed a recv's bytes to a promoted conn's dispatch goroutine, with the
// header deadline the feed read before it handed them over (hdrDL), and before
// the feed arms the header timer from it. A test counts the calls to see that
// a recv took the feed, which the conn's responses alone do not show (the
// inline path serves the same bytes); reads cs.headerTimerArmed, which is the
// worker's, to see whether an earlier feed armed the kernel timer; and, as
// the h2c race test does, tells the dispatch goroutine that the feed is over.
// It sits before the unlocked read that celeris#722 removed from this site, so
// that a test can hold the goroutine until the worker has reached the point
// where that read was. Read through an atomic pointer so a test arming it
// after the engine started is not a data race. One load per feed.
var asyncFeedHook atomic.Pointer[func(w *Worker, cs *connState, hdrDL int64)]

// h2cUpgradeHook is a test seam (celeris#884 item 83): nil in production, it
// runs in switchToH2Local, holding cs.detachMu, immediately before the upgrade
// sets cs.h1State to nil. switchToH2Local is reached from the dispatch
// goroutine (an upgrade that came through the async path) and from the
// worker's own inline upgrade (switchToH2): a hook that blocks parks whichever
// thread runs it, the worker included, so one that waits must be armed only
// where the upgrade is the goroutine's. A test holds the goroutine here, so that its
// write of cs.h1State is the next thing it does, and releases it once the
// worker's feed has run: a worker read of cs.h1State after that feed is then
// concurrent with the write in the race detector's sense, whatever the
// scheduler does (the race detector reports unordered accesses, not
// simultaneous ones). The upgrade is rare and already allocates; the load is
// not on a request's path.
var h2cUpgradeHook atomic.Pointer[func(cs *connState)]
