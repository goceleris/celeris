//go:build linux

package iouring

import "sync/atomic"

// detectStage is a point in handleRecv's detection branch, between the
// moment Detect has decided a conn's protocol and the moment the conn is set
// up for it.
type detectStage uint8

const (
	// detectBeforeBudgetCheck is right before the check of the budget the
	// conn was given at accept.
	detectBeforeBudgetCheck detectStage = iota
	// detectAfterBudgetCheck is right after that check passed and before
	// initProtocol, which arms the header deadline.
	detectAfterBudgetCheck
)

// detectStageHook is a fault-injection seam, nil in production and set only by
// tests (as the seams of abort_seam_844.go are). The budget check and the
// re-check after initProtocol are decided by the clock at the instant they
// run, and a conn that reaches them late is closed by the idle sweep first
// unless the late bytes land inside the sweep's period, a race no test can win
// reliably under load. The hook runs at both stages and may move
// cs.detectDeadline, which puts a conn through either exit on demand. It is
// read through an atomic pointer so that a test arming it after the engine
// started is not a data race, and costs one load per conn whose protocol had
// to be detected, never one per request.
var detectStageHook atomic.Pointer[func(w *Worker, cs *connState, st detectStage)]
