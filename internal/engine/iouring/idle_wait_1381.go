//go:build linux

package iouring

// The long wait of a worker that has nothing to serve (celeris#1381).
//
// An idle worker's ring wait is 100 ms (Worker.baseTimeout), but a
// ReadHeaderTimeout above zero (the default is 10 s) capped it at 25 ms, with
// no connection for a header deadline to watch: about 40 wake-ups a second per
// worker, whatever the traffic. The cap is for the deadlines of the connections
// a worker holds (the header timer is a kernel timer, and the cap backs it up
// when its SQE could not be armed under SQ-ring pressure), so a worker that holds
// no connection of any kind, and has nothing queued, keeps the 100 ms wait. With
// any connection the cap is what it was.
//
// What ends the wait early is what ends any ring wait: a completion (an accept,
// a recv), or a write of the wake eventfd, whose POLL_ADD the loop keeps armed in
// every mode, and which every producer of work for the worker makes: the driver
// actions (register, unregister, write, adopt), the detach queue, the h2 write
// queue, BeginPauseAccept, ResumeAccept, StartTransplant and the cancel of the
// engine's context. The wait stays bounded, so a producer that is added without
// a wake costs one 100 ms wait, not a hang. The worker with the 100 ms wait is
// not new: it is every worker whose ReadHeaderTimeout is -1.

// idleQuiescent reports whether the worker holds nothing and has nothing queued:
// no connection of any kind, no work owed to it, no drain, pause or linger in
// progress, the wake eventfd and its armed poll, and no counter or residue left
// to publish. Worker thread only. The facts are the ones the DRAINING to
// SUSPENDED park tests, plus the open listener and the wake.
//
// What counts as live: connCount and liveConns hold every HTTP conn the worker
// owns, HTTP/2 and detached (WebSocket, SSE) ones included (they leave only in
// finishClose and finishCloseDetached); a hijacked conn and one handed to the
// other engine have left them. Driver conns are in no count: hasDriverConns.
func (w *Worker) idleQuiescent() bool {
	return w.listenFD >= 0 && w.wakeFD.FD() >= 0 && w.h2PollArmed &&
		w.connCount == 0 && len(w.liveConns) == 0 && len(w.h2Conns) == 0 && w.detachedCount == 0 &&
		w.dirtyHead == nil && w.lingerUntil == 0 && w.closeFDOwed == 0 &&
		!w.draining && w.h2DrainStart == 0 && !w.acceptPaused.Load() &&
		w.transplant.Load() == nil && w.sweepPub == ([numResidual]uint64{}) &&
		!w.hasDriverConns.Load() && w.driverActionPending.Load() == 0 && w.detachQPending.Load() == 0 &&
		w.reqBatch == 0 && w.bytesReadBatch == 0 && w.bytesWrittenBatch == 0 && w.ringBytesBatch == 0 && w.linkArmBatch == 0
}
