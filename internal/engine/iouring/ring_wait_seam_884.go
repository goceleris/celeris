//go:build linux

package iouring

// ringWaitMode names which of Worker.run's ring waits ended: the argument the
// seam below receives, so a test can say which wait of the loop it answered.
type ringWaitMode uint8

const (
	// waitModeSQPoll is WaitCQETimeout, the SQPOLL worker's wait.
	waitModeSQPoll ringWaitMode = iota
	// waitModeSubmitReady is SubmitAndWait with CQEs already ready and SQEs
	// pending (mode 1 of the loop's submit block).
	waitModeSubmitReady
	// waitModeSubmitSends is SubmitAndWait with SENDs pending (mode 3a).
	waitModeSubmitSends
	// waitModeTimed is SubmitAndWaitTimeout (mode 3b).
	waitModeTimed
)

// String names the wait for the log line of a retried one.
func (m ringWaitMode) String() string {
	switch m {
	case waitModeSQPoll:
		return "sqpoll-wait"
	case waitModeSubmitReady:
		return "submit-and-wait (completions ready)"
	case waitModeSubmitSends:
		return "submit-and-wait (sends pending)"
	case waitModeTimed:
		return "submit-and-wait-timeout"
	}
	return "unknown"
}

// ringWaitSeam is a test seam (celeris#884 item 49): when set, Worker.run calls
// it after each of its ring waits, on the worker's thread, with the wait's own
// result, and acts on what it returns instead. A test answers EBUSY the way a
// 5.19 to 6.1 kernel does for a CQ ring it cannot flush (a kernel the CI and
// the laptop do not run), or counts the waits to prove an injection reached
// the loop. Several workers call it, so a test keeps its state atomic. Nil in
// production, where each wait costs one load of this variable.
var ringWaitSeam func(w *Worker, mode ringWaitMode, err error) error
