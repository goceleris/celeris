package redis

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// How execManyDirect waits for the first bytes of a pipeline's reply
// (celeris#854). A direct-mode conn can park the goroutine in Go's netpoll
// (cheap when the reply is quick) or wait in poll(2) on the caller's own
// thread (cheaper when the round trip is long: no netpoll wake-up hop). The
// switch below picks one for the whole process; the counters say what each
// Exec did. Both are unexported on purpose: no public API, and the tests and a
// probe built into the package read them.

// directWaitKind is the wait-mode switch.
type directWaitKind int32

const (
	// directWaitUnset means the environment has not been read yet.
	directWaitUnset directWaitKind = -1
	// directWaitNetpoll always parks in Go's netpoll (the behaviour before
	// #854). It is the default.
	directWaitNetpoll directWaitKind = 0
	// directWaitAuto lets each conn choose from its own first-chunk wait.
	directWaitAuto directWaitKind = 1
	// directWaitBlock always waits in poll(2) (where the platform has it).
	directWaitBlock directWaitKind = 2
)

// directWaitEnv names the environment variable that sets the mode: "netpoll"
// (the default), "auto" or "block", compared after trimming spaces and folding
// case. Any other value means netpoll: a mistyped setting never switches the
// feature on. It is read on the first Exec that needs it, not at program start,
// so a program may os.Setenv it first.
const directWaitEnv = "CELERIS_REDIS_DIRECT_WAIT"

// directWaitMode is the process-wide mode, directWaitUnset until first use;
// tests set it through setDirectWaitForTest. directWaitOnce guards the read.
var (
	directWaitMode atomic.Int32
	directWaitOnce sync.Once
)

// init only stores the "not read yet" sentinel; the environment is not read here.
func init() { directWaitMode.Store(int32(directWaitUnset)) }

// currentDirectWait returns the mode, reading the environment on first use.
func currentDirectWait() directWaitKind {
	if m := directWaitKind(directWaitMode.Load()); m != directWaitUnset {
		return m
	}
	directWaitOnce.Do(func() {
		directWaitMode.CompareAndSwap(int32(directWaitUnset), int32(parseDirectWait(os.Getenv(directWaitEnv))))
	})
	if m := directWaitKind(directWaitMode.Load()); m != directWaitUnset {
		return m
	}
	return directWaitNetpoll // a test reset the mode between the two loads
}

// parseDirectWait maps the environment value to a mode; unknown means netpoll.
func parseDirectWait(s string) directWaitKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return directWaitAuto
	case "block":
		return directWaitBlock
	default:
		return directWaitNetpoll
	}
}

// paddedCounter keeps one counter on a cache line of its own: the counters are
// process-wide and written from every Exec, so sharing a line would make the
// Execs of different conns contend on each other's counters.
type paddedCounter struct {
	_ [64]byte
	atomic.Uint64
	_ [56]byte
}

// directWaitCounters count what the direct pipeline path did, process-wide.
type directWaitCounters struct {
	// blocks counts waits started in poll(2): one per episode of waiting for
	// bytes, not one per poll call.
	blocks paddedCounter
	// sliceExpiries counts poll(2) slices that ended with nothing to read and
	// were polled again. Informational: it says how long the thread waited.
	sliceExpiries paddedCounter
	// handoffs counts the waits that used up directPollBudget in poll(2) and
	// were handed to Go's netpoll (tcp.Read) with the reply still not there.
	handoffs paddedCounter
	// netpollParks counts the reads handed to Go's netpoll (tcp.Read) after the
	// non-blocking peek found nothing, handoffs included, by waits in auto or
	// block mode. The default netpoll mode runs the pre-#854 loop and counts
	// nothing: blocks and handoffs staying 0 is how a test sees it did not block.
	netpollParks paddedCounter
}

// directWaitStats is the process-wide instance.
var directWaitStats directWaitCounters

// directWaitSnap is a copy of the counters.
type directWaitSnap struct{ Blocks, SliceExpiries, Handoffs, NetpollParks uint64 }

// directWaitSnapshot reads the counters.
func directWaitSnapshot() directWaitSnap {
	return directWaitSnap{
		Blocks:        directWaitStats.blocks.Load(),
		SliceExpiries: directWaitStats.sliceExpiries.Load(),
		Handoffs:      directWaitStats.handoffs.Load(),
		NetpollParks:  directWaitStats.netpollParks.Load(),
	}
}

// since returns the counters' growth since the earlier snapshot s.
func (s directWaitSnap) since(e directWaitSnap) directWaitSnap {
	return directWaitSnap{s.Blocks - e.Blocks, s.SliceExpiries - e.SliceExpiries, s.Handoffs - e.Handoffs, s.NetpollParks - e.NetpollParks}
}
