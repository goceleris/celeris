// Package errclass carries the per-cause breakdown behind
// [github.com/goceleris/celeris/engine.EngineMetrics.ErrorCount].
//
// ErrorCount used to be a single atomic that a dozen unrelated branches
// incremented: an accept that hit EMFILE, a descriptor past the worker's
// conn-table cap, an epoll_ctl registration failure, a refused transplant
// adoption, a failed send completion, a request body that would not read.
// celeris#645 measured 421 of them on one adaptive cell against 63 for the
// same refapp on io_uring and 0 on epoll, and the counter could not say which
// of those branches produced even one of them — only that the two sub-engines
// on their own did not account for the total.
//
// So the buckets, not the total, are the state now. [Counters] holds one
// atomic per cause and [Counters.Total] is the sum; no engine keeps a separate
// running total that could drift from the parts, and there is no generic
// "bump the error count" call left to reach for. A new error branch has to
// name a bucket, and if it needs a new one, TestTotalCoversEveryBucket fails
// until Total sums it too.
//
// Which errno lands in which accept bucket is [Counters.AcceptFailed]'s job,
// so epoll and io_uring classify the same failure the same way.
package errclass

import "sync/atomic"

// Counters is an engine's live per-cause error tally. Always held by pointer:
// it contains atomics and must never be copied. The zero value is ready to use.
//
// Every field here is an ErrorCount bucket and is surfaced as its own
// EngineMetrics field. Engines that cannot reach a cause simply leave it zero
// (epoll never sends through a completion queue, std never accepts a
// descriptor of its own), which is what makes the split readable across a
// three-engine matrix column: a bucket that is nonzero on exactly one engine
// is a property of that engine, not of the load.
type Counters struct {
	// AcceptFDLimit counts accepts refused for want of a descriptor:
	// EMFILE (per-process) or ENFILE (system-wide).
	AcceptFDLimit atomic.Uint64
	// AcceptCancelled counts accept failures that mean the accept itself
	// went away rather than the host running out of something: ECANCELED
	// (an in-flight accept was cancelled — what a PauseAccept does), EBADF
	// (the listen descriptor was closed under an accept), ECONNABORTED (the
	// peer reset between SYN and accept) and EINTR.
	//
	// io_uring reports all four through a completion and counts them here.
	// epoll's accept4 loop retries ECONNABORTED and EINTR in place and has
	// never counted them at all, so its share of this bucket is only ever
	// the failures it does not retry. That asymmetry is deliberate for now:
	// folding epoll's retries in would change ErrorCount's value on every
	// published epoll column. It is also the reason an epoll cell can read
	// 0 while an io_uring cell on the same refapp does not.
	AcceptCancelled atomic.Uint64
	// AcceptOther counts every accept failure that is neither of the above.
	AcceptOther atomic.Uint64
	// ConnTableCap counts descriptors dropped because they fall outside the
	// worker's flat connection table — the epoll conn-table cap, and
	// io_uring's equivalent bound — on both the accept and the transplant
	// adoption path. The descriptor is closed; the connection is lost.
	ConnTableCap atomic.Uint64
	// ConnRegister counts descriptors dropped because registering them with
	// the event loop failed (epoll_ctl ADD), again on both the accept and
	// the adoption path. epoll only.
	ConnRegister atomic.Uint64
	// ListenerRecreate counts failures to re-create a listen socket after a
	// ResumeAccept. The loop or worker that hits one shuts itself down, so
	// a nonzero value is an engine that has lost accept capacity, not a
	// transient.
	ListenerRecreate atomic.Uint64
	// TransplantAdopt counts adoptions refused because the target's
	// conn-table slot for that descriptor was already occupied. Matches
	// EngineMetrics.TransplantAdoptSlotOccupied one for one (celeris#624);
	// it is counted in both places because it is both a lost hand-off and
	// an error, and #624 wants the hand-off ledger to stand on its own.
	TransplantAdopt atomic.Uint64
	// SendPeerGone counts send completions that failed because the peer
	// was already gone — EPIPE, ECONNRESET, ECONNABORTED, ENOTCONN. It is
	// split out from Send because it is not a fault of the server at all:
	// it is one count per connection whose client stopped reading before
	// the response flushed, so it tracks how often clients abandon
	// requests, and it scales with load and with client timeouts rather
	// than with anything the engine did wrong.
	//
	// io_uring only, and that is the point. epoll's write path reports a
	// dead peer through the handler's OnError and has never fed ErrorCount
	// at all, so the same abandoned request costs io_uring one ErrorCount
	// and epoll zero. Any epoll-vs-io_uring ErrorCount comparison is
	// dominated by this bucket (celeris#645).
	SendPeerGone atomic.Uint64
	// Send counts send completions that failed for any other reason — a
	// genuine transmit fault rather than a client that left. io_uring only,
	// for the same reason as SendPeerGone.
	Send atomic.Uint64
	// RequestBody counts requests rejected before the handler ran because
	// the body would not read or exceeded MaxRequestBodySize. std only.
	RequestBody atomic.Uint64
	// Handler counts handler invocations that returned an error. std only:
	// the native engines do not fold a handler error into ErrorCount.
	Handler atomic.Uint64
}

// Snapshot is a plain-value copy of [Counters], one uint64 per bucket, in the
// same order. Engines build one per Metrics() call.
type Snapshot struct {
	AcceptFDLimit    uint64
	AcceptCancelled  uint64
	AcceptOther      uint64
	ConnTableCap     uint64
	ConnRegister     uint64
	ListenerRecreate uint64
	TransplantAdopt  uint64
	SendPeerGone     uint64
	Send             uint64
	RequestBody      uint64
	Handler          uint64
}

// Snapshot reads every bucket. The reads are individually atomic but not
// mutually consistent, which is the same guarantee every other counter in
// EngineMetrics gives.
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		AcceptFDLimit:    c.AcceptFDLimit.Load(),
		AcceptCancelled:  c.AcceptCancelled.Load(),
		AcceptOther:      c.AcceptOther.Load(),
		ConnTableCap:     c.ConnTableCap.Load(),
		ConnRegister:     c.ConnRegister.Load(),
		ListenerRecreate: c.ListenerRecreate.Load(),
		TransplantAdopt:  c.TransplantAdopt.Load(),
		SendPeerGone:     c.SendPeerGone.Load(),
		Send:             c.Send.Load(),
		RequestBody:      c.RequestBody.Load(),
		Handler:          c.Handler.Load(),
	}
}

// Total is EngineMetrics.ErrorCount: the sum of every bucket. It is derived
// rather than stored, so the published total and the published parts cannot
// disagree. TestTotalCoversEveryBucket walks Snapshot by reflection and fails
// if a bucket is missing from this sum.
func (s Snapshot) Total() uint64 {
	return s.AcceptFDLimit + s.AcceptCancelled + s.AcceptOther +
		s.ConnTableCap + s.ConnRegister + s.ListenerRecreate +
		s.TransplantAdopt + s.SendPeerGone + s.Send + s.RequestBody + s.Handler
}

// Total is the sum of every bucket read straight off the live counters.
func (c *Counters) Total() uint64 { return c.Snapshot().Total() }
