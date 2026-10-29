package stream

import (
	"time"

	"golang.org/x/net/http2"
)

// The per-stream request deadline and the header-block deadline (celeris#889).
//
// A request that is not whole ReadTimeout after its HEADERS is answered 408 and
// reset with RST_STREAM(NO_ERROR), the connection and its other streams go on
// (net/http's model: RFC 9113 section 8.1 lets a server answer before the
// request is whole and then reset the stream with NO_ERROR). A header block
// that is not complete ReadHeaderTimeout after its first HEADERS frame ends the
// connection with GOAWAY(ENHANCE_YOUR_CALM): HPACK state is the connection's, so
// a half-read block cannot be skipped. Both are armed on the loop thread under
// H2State.mu; the engines' sweep expires them (conn.H2State.Sweep).

// requestTombstoneFactor is how long, in request timeouts, the server remembers
// a request it expired when the client does not finish it: until the client's
// END_STREAM, or this long.
const requestTombstoneFactor = 2

// maxRequestTombstones bounds the memory of expired requests: a client cannot
// open more than MAX_CONCURRENT_STREAMS requests per ReadTimeout and each is
// remembered for requestTombstoneFactor of them, so this is the bound that
// honest traffic cannot reach; past it the oldest are forgotten and the
// ordinary reset window (resetIDs) is all that is left.
const maxRequestTombstones = 1024

// requestExpired reports whether stream id is one whose request deadline expired
// and whose client has not ended it yet: its DATA and trailers, in flight, are
// ignored whatever the age of the stream (resetIDs forgets a reset that is more
// than serverResetWindow identifiers old, and the uploader of an old stream is by
// definition still sending). Loop thread, under H2State.mu.
func (p *Processor) requestExpired(id uint32) bool {
	_, ok := p.expiredReq[id]
	return ok
}

// forgetExpired drops the memory of an expired request whose client has ended it.
func (p *Processor) forgetExpired(id uint32) {
	if p.expiredReq != nil {
		delete(p.expiredReq, id)
	}
}

// ExpireRequest answers request id, whose deadline passed, 408 and resets it with
// RST_STREAM(NO_ERROR), as a server that answers before the request is whole may
// (RFC 9113 section 8.1). The stream is remembered (markResetOpen, and the
// tombstone) until its client ends it: the DATA it has in flight is ignored and
// credited to the connection's window, the trailers are dropped, the connection
// is not ended for them. It is not a refusal and is not counted as one: expiries
// are bounded by MAX_CONCURRENT_STREAMS per ReadTimeout. The handler has not run
// (the native path runs it once the request is whole), so there is nothing to
// race: no headers are out. Called by the loop thread under H2State.mu, which is
// what the inline writer needs (it writes the connection's buffer directly).
func (p *Processor) ExpireRequest(id uint32, now int64) {
	s, ok := p.manager.GetStream(id)
	if !ok {
		return
	}
	s.Cancel()
	w := p.InlineWriter
	if w == nil {
		w = p.connWriter
	}
	if w != nil {
		_ = w.WriteResponse(s, 408, [][2]string{{"content-length", "0"}}, nil)
	}
	p.markResetOpen(id, false)
	if p.expiredReq == nil {
		p.expiredReq = make(map[uint32]int64)
	}
	if len(p.expiredReq) >= maxRequestTombstones {
		p.dropOldestTombstone()
	}
	until := now + requestTombstoneFactor*int64(p.manager.reqTimeout)
	p.expiredReq[id] = until
	p.noteTombstoneDeadline(until)
	p.resetIDs.mark(id)
	if p.connWriter != nil {
		// As sendRSTStreamAndMarkClosed does, for a writer that records closed
		// streams; the engines' adapters of today keep no such record (their
		// MarkStreamClosed is empty), so this is a no-op there, and what keeps a
		// late write for the stream from following the reset is the stream's
		// removal from the manager just below.
		p.connWriter.MarkStreamClosed(id)
	}
	if err := p.writer.WriteRSTStream(id, http2.ErrCodeNo); err == nil {
		p.flush()
	}
	p.manager.DeleteStream(id)
}

func (p *Processor) noteTombstoneDeadline(until int64) {
	for {
		cur := p.manager.hints.TombDL.Load()
		if cur != 0 && cur <= until {
			return
		}
		if p.manager.hints.TombDL.CompareAndSwap(cur, until) {
			return
		}
	}
}

func (p *Processor) dropOldestTombstone() {
	var oldest uint32
	var at int64
	for id, t := range p.expiredReq {
		if at == 0 || t < at {
			oldest, at = id, t
		}
	}
	delete(p.expiredReq, oldest)
}

// NextTombstoneDeadline is the instant the first expired-request memory runs out,
// or 0. An atomic load for the sweep.
func (p *Processor) NextTombstoneDeadline() int64 { return p.manager.hints.TombDL.Load() }

// DropTombstones forgets the expired requests whose memory has run out at now and
// settles the hint. Loop thread, under H2State.mu.
func (p *Processor) DropTombstones(now int64) {
	var next int64
	for id, t := range p.expiredReq {
		if t <= now {
			delete(p.expiredReq, id)
			continue
		}
		if next == 0 || t < next {
			next = t
		}
	}
	p.manager.hints.TombDL.Store(next)
}

// ignoredFrame notes that a frame of an expired request's stream was ignored: the
// client is still sending that request, which the connection's idle clock counts
// as activity (conn.H2State.Sweep). It is called only for a stream with a
// tombstone, whose memory lasts at most 2 x ReadTimeout from the expiry: the slow
// uploader's DATA and trailers in flight (M3) are activity for that long and no
// longer. A frame for a stream the server reset for another cause, or one that is
// in the window of reset identifiers only, is ignored and is not activity: a
// client could otherwise send a byte of DATA on such a stream every period for as
// long as it liked, at no cost, and hold the idle clock and the progress cap's
// baseline (celeris#889, review r5 F2). A cold path.
func (p *Processor) ignoredFrame() { p.ignoredAct.Store(true) }

// TakeIgnoredActivity reports, and clears, whether a frame was ignored for a
// reset or expired stream since the last call.
func (p *Processor) TakeIgnoredActivity() bool {
	if !p.ignoredAct.Load() {
		return false
	}
	p.ignoredAct.Store(false)
	return true
}

// SetHeaderBlockTimeout sets the deadline of a header block (the server's
// ReadHeaderTimeout): ReadHeaderTimeout after the first HEADERS frame of a block
// that does not end with it, the connection ends with GOAWAY(ENHANCE_YOUR_CALM).
// 0 turns it off. Set once, before the first frame.
func (p *Processor) SetHeaderBlockTimeout(d time.Duration) { p.hdrTimeout = clampTimeout(d) }

// armHeaderBlock starts the deadline of the header block that has just begun
// (a HEADERS frame without END_HEADERS).
func (p *Processor) armHeaderBlock() {
	if p.hdrTimeout > 0 {
		p.manager.hints.HdrDL.Store(time.Now().UnixNano() + int64(p.hdrTimeout))
	}
}

// disarmHeaderBlock ends it: the block is complete, or the connection is over.
func (p *Processor) disarmHeaderBlock() {
	if p.manager.hints.HdrDL.Load() != 0 {
		p.manager.hints.HdrDL.Store(0)
	}
}

// NextHeaderBlockDeadline is the instant the pending header block runs out of
// time, or 0 if none is pending. An atomic load for the sweep.
func (p *Processor) NextHeaderBlockDeadline() int64 { return p.manager.hints.HdrDL.Load() }

// ExpireHeaderBlock ends the connection whose header block was not completed in
// time: GOAWAY(ENHANCE_YOUR_CALM), naming the last stream the server opened. The
// caller closes the connection once the GOAWAY is out. It is not PROTOCOL_ERROR:
// the peer's frames so far were well formed, it was slow.
func (p *Processor) ExpireHeaderBlock() {
	p.disarmHeaderBlock()
	_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeEnhanceYourCalm, []byte("header block not completed in time"))
}
