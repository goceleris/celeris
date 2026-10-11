//go:build linux

package iouring

// benchWorkerHook896 gives a bench worker what the PR added to Worker (the
// handoff-loss counters that stopAccepting's give-up path notes). The copy of
// this file in scripts/mkmain.sh's origin/main tree has an empty body: the
// field does not exist there.
func benchWorkerHook896(w *Worker) { w.handoffLoss = &handoffLossStats{} }
