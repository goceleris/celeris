//go:build linux

package iouring

// resetAsyncCancelProbeCache forgets the async-cancel-flags probe's cached
// answer, so the next probeAsyncCancelFlagsCached (and so the next New)
// probes again. Tests only.
func resetAsyncCancelProbeCache() {
	asyncCancelMu.Lock()
	defer asyncCancelMu.Unlock()
	forgetAsyncCancelAnswerLocked()
}
