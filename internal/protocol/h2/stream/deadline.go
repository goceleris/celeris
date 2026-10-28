package stream

import (
	"golang.org/x/net/http2"
)

// The per-stream request deadline and the header-block deadline (celeris#889).
//
// A request that is not whole ReadTimeout after its HEADERS is answered 408 and
// reset with RST_STREAM(NO_ERROR), the connection and its other streams go on
// (net/http's model: RFC 9113 section 8.1 lets a server answer before the
// request is whole and then reset the stream with NO_ERROR). It is armed on the
// loop thread under H2State.mu; the engines' sweep expires it (conn.H2State.Sweep).

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
	if err := p.writer.WriteRSTStream(id, http2.ErrCodeNo); err == nil {
		p.flush()
	}
	p.manager.DeleteStream(id)
}

func (p *Processor) noteTombstoneDeadline(until int64) {
	for {
		cur := p.tombDL.Load()
		if cur != 0 && cur <= until {
			return
		}
		if p.tombDL.CompareAndSwap(cur, until) {
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
func (p *Processor) NextTombstoneDeadline() int64 { return p.tombDL.Load() }

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
	p.tombDL.Store(next)
}
