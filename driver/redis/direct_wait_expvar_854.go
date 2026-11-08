package redis

import (
	"expvar"
	"os"
)

// DIAGNOSTIC for celeris#854, never merged: the direct-wait mode and counters
// as one expvar, so a benchmark harness that serves expvar (or reads
// expvar.Get) can report them before and after a measured window. Counters
// exist on every platform; off linux they stay 0 because every wait parks in
// netpoll. The mode is read WITHOUT latching: calling currentDirectWait here
// would read CELERIS_REDIS_DIRECT_WAIT early and freeze it before a program's
// own os.Setenv, so a probe could change what it measures.

func init() { expvar.Publish("celeris_redis_directwait", expvar.Func(directWaitVar)) }

// directWaitVar returns the mode and a snapshot of the counters.
func directWaitVar() any {
	mode := "unread"
	switch directWaitKind(directWaitMode.Load()) {
	case directWaitNetpoll:
		mode = "netpoll"
	case directWaitAuto:
		mode = "auto"
	case directWaitBlock:
		mode = "block"
	}
	s := directWaitSnapshot()
	return map[string]any{
		"mode":            mode,
		"env":             os.Getenv(directWaitEnv),
		"block_supported": directBlockSupported,
		"blocks":          s.Blocks,
		"slice_expiries":  s.SliceExpiries,
		"handoffs":        s.Handoffs,
		"netpoll_parks":   s.NetpollParks,
	}
}
