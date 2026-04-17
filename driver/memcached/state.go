package memcached

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// bytesToString returns a string that aliases b — no copy. Safe only when
// b is owned by the caller and will not be mutated for the string's
// lifetime. We use it in GetMulti's output path: the []byte came from
// copyBytes (owned) and is never written again; the returned string is
// handed to the caller's map and becomes the sole reference to the
// backing array.
func bytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// replyKind tells the parser how to interpret the sequence of wire-level
// replies that terminate one logical request. Memcached protocols do not
// carry an explicit correlation ID (the opaque field exists in binary but
// is optional), so dispatch relies on request-order FIFO plus the caller-
// specified expected shape.
type replyKind uint8

const (
	// kindStatusOnly: one line, a single STORED / NOT_STORED / EXISTS /
	// NOT_FOUND / DELETED / TOUCHED / OK / ERROR / CLIENT_ERROR / SERVER_ERROR.
	kindStatusOnly replyKind = iota
	// kindGet: zero or more VALUE blocks terminated by END. Used by get,
	// gets, gat, gats.
	kindGet
	// kindArith: a bare number (incr/decr success) or NOT_FOUND / ERROR /
	// CLIENT_ERROR / SERVER_ERROR.
	kindArith
	// kindVersion: "VERSION <version>\r\n" or an ERROR.
	kindVersion
	// kindStats: zero or more STAT lines terminated by END.
	kindStats
	// kindBinary: one binary packet (Opcode matches the request) or a binary
	// error packet with a non-zero Status. Used by every binary-protocol op.
	kindBinary
	// kindBinaryMulti: a binary request that elicits a stream of response
	// packets terminated by a sentinel — OpGetKQ pipelines terminated by an
	// OpNoop echo (GetMulti), or an OpStat stream terminated by a zero-length
	// key packet. The accumulator predicate [mcRequest.binIsTerm] decides when
	// the stream is done.
	kindBinaryMulti
)

// valueItem is one VALUE reply (or binary GET hit) copied into request-owned
// memory. The protocol.TextReader aliases its backing buffer, so dispatch
// copies Data before signaling completion.
//
// Key is stored as a string rather than []byte: GetMulti's output map is
// keyed by string, so storing Key as string folds the unavoidable []byte →
// string conversion into dispatch (one alloc) instead of forcing the
// consuming loop to do it later (which would require a second copy). Data
// stays []byte because GetMultiBytes wants the raw bytes and Get's one
// extra string(Data) conversion happens at the caller without a second
// copy (Go 1.26 recognises the immediate-use pattern).
type valueItem struct {
	Key   string
	Flags uint32
	CAS   uint64
	Data  []byte
}

// mcRequest is one in-flight memcached command awaiting a reply. Callers
// populate `kind` plus any extra context (e.g. `opaque` for binary) before
// enqueueing; the parser fills in the result fields when the matching reply
// arrives.
type mcRequest struct {
	ctx context.Context

	kind replyKind
	// opaque is the binary-protocol correlation ID expected back. Ignored in
	// text mode.
	opaque uint32

	// Result fields — populated by the parser.
	values    []valueItem
	stats     []statItem
	number    uint64
	version   string
	status    protocol.Kind // text-protocol reply tag (ignored in binary mode)
	binStatus uint16        // binary-protocol status code
	resultErr error

	// binIsTerm reports whether p is the terminator packet for a
	// kindBinaryMulti request. Non-terminator packets are accumulated into
	// req.values (GetMulti hits) or req.stats (Stats entries). Set by the
	// caller before enqueueing; ignored on non-multi kinds.
	binIsTerm func(p protocol.BinaryPacket) bool

	doneCh   chan struct{}
	finished atomic.Bool
}

// statItem is one STAT line from a stats reply.
type statItem struct {
	Name  string
	Value string
}

// finish signals the request as complete exactly once. Safe to call from
// multiple goroutines. doneCh is buffered (cap=1); the nonblocking send wakes
// the (single) waiter without allocating.
func (r *mcRequest) finish() {
	if r.finished.CompareAndSwap(false, true) {
		select {
		case r.doneCh <- struct{}{}:
		default:
		}
	}
}

// Ctx implements async.PendingRequest.
func (r *mcRequest) Ctx() context.Context { return r.ctx }

// reqPool recycles mcRequest values — allocating one per command dominates
// allocation budgets otherwise. doneCh is a buffered channel with capacity 1;
// finish() sends into it (instead of closing it) so the channel itself can be
// reused across pool cycles without reallocation.
var reqPool = sync.Pool{
	New: func() any {
		return &mcRequest{doneCh: make(chan struct{}, 1)}
	},
}

// getRequest returns a zeroed mcRequest with a drained, reusable doneCh.
func getRequest(ctx context.Context) *mcRequest {
	r := reqPool.Get().(*mcRequest)
	r.ctx = ctx
	r.kind = kindStatusOnly
	r.opaque = 0
	r.values = r.values[:0]
	r.stats = r.stats[:0]
	r.number = 0
	r.version = ""
	r.status = 0
	r.binStatus = 0
	r.resultErr = nil
	r.binIsTerm = nil
	r.finished.Store(false)
	return r
}

// putRequest returns r to the pool. The doneCh on r is buffered (cap=1); if
// finish() sent an item and no waiter received it (e.g. ctx-cancel races),
// drain it here so the next acquirer sees an empty channel.
func putRequest(r *mcRequest) {
	if r == nil {
		return
	}
	select {
	case <-r.doneCh:
	default:
	}
	r.ctx = nil
	// Zero-length the result slices before returning; retain the backing
	// arrays for reuse. Callers own their `values[i].Data` copies, so
	// resetting the header is sufficient.
	for i := range r.values {
		r.values[i] = valueItem{}
	}
	r.values = r.values[:0]
	for i := range r.stats {
		r.stats[i] = statItem{}
	}
	r.stats = r.stats[:0]
	reqPool.Put(r)
}

// mcState is the event-loop-side decoder shared by a single memcachedConn.
// Depending on Config.Protocol only one of textR/binR is non-nil; the writer
// mirrors that choice.
type mcState struct {
	textR *protocol.TextReader
	textW *protocol.TextWriter
	binR  *protocol.BinaryReader
	binW  *protocol.BinaryWriter

	binary bool

	bridge *async.Bridge

	// pendingGet is the in-progress get/gat request consuming VALUE blocks
	// until END. Using a pointer here (instead of bridge.Head) avoids a
	// mutex acquisition per VALUE line — the field is only touched from the
	// single worker goroutine that runs processRecv.
	pendingGet *mcRequest
	// pendingStats is the in-progress stats request consuming STAT lines.
	pendingStats *mcRequest
}

// newMCState returns a state machine configured for the given protocol.
func newMCState(bin bool) *mcState {
	s := &mcState{bridge: async.NewBridge(), binary: bin}
	if bin {
		s.binR = protocol.NewBinaryReader()
		s.binW = protocol.NewBinaryWriter()
	} else {
		s.textR = protocol.NewTextReader()
		s.textW = protocol.NewTextWriter()
	}
	return s
}

// processRecv feeds bytes and drains complete replies. Returns a non-nil
// error on protocol violation; the caller closes the conn.
//
// It runs on the worker goroutine and MUST NOT block.
func (s *mcState) processRecv(data []byte) error {
	if s.binary {
		return s.processBinary(data)
	}
	return s.processText(data)
}

func (s *mcState) processText(data []byte) error {
	s.textR.Feed(data)
	for {
		reply, err := s.textR.Next()
		if errors.Is(err, protocol.ErrIncomplete) {
			break
		}
		if err != nil {
			return err
		}
		if derr := s.dispatchText(reply); derr != nil {
			return derr
		}
	}
	s.textR.Compact()
	return nil
}

// dispatchText routes one text-protocol reply.
func (s *mcState) dispatchText(v protocol.TextReply) error {
	// Multi-line replies (get / stats) are assembled before the terminating
	// END pops the bridge.
	switch v.Kind {
	case protocol.KindValue:
		req := s.pendingGet
		if req == nil {
			// Speculatively match against the head of the bridge — tests
			// that skip the request setup shouldn't crash the parser, but
			// real callers always install kindGet before writing the cmd.
			head := s.bridge.Head()
			if head == nil {
				return errors.New("celeris/memcached: unexpected VALUE with empty queue")
			}
			req = head.(*mcRequest)
			s.pendingGet = req
		}
		item := valueItem{
			Key:   string(v.Key),
			Flags: v.Flags,
			CAS:   v.CAS,
			Data:  copyBytes(v.Data),
		}
		req.values = append(req.values, item)
		return nil
	case protocol.KindEnd:
		if s.pendingGet != nil {
			// Terminates a get/gat reply.
			req := s.pendingGet
			s.pendingGet = nil
			if popped := s.bridge.Pop(); popped != req {
				// Bridge head drift would indicate a framing bug — fail loud.
				return errors.New("celeris/memcached: bridge head mismatch on END")
			}
			req.status = protocol.KindEnd
			req.finish()
			return nil
		}
		if s.pendingStats != nil {
			req := s.pendingStats
			s.pendingStats = nil
			if popped := s.bridge.Pop(); popped != req {
				return errors.New("celeris/memcached: bridge head mismatch on END")
			}
			req.status = protocol.KindEnd
			req.finish()
			return nil
		}
		// A stand-alone END on a get with no VALUE blocks lands here; pop
		// the head and resolve it as an empty result.
		head := s.bridge.Pop()
		if head == nil {
			return errors.New("celeris/memcached: unexpected END with empty queue")
		}
		req := head.(*mcRequest)
		req.status = protocol.KindEnd
		req.finish()
		return nil
	case protocol.KindStat:
		req := s.pendingStats
		if req == nil {
			head := s.bridge.Head()
			if head == nil {
				return errors.New("celeris/memcached: unexpected STAT with empty queue")
			}
			req = head.(*mcRequest)
			s.pendingStats = req
		}
		req.stats = append(req.stats, statItem{
			Name:  string(v.Key),
			Value: string(v.Data),
		})
		return nil
	}

	// Single-line replies terminate the head-of-queue request.
	head := s.bridge.Pop()
	if head == nil {
		return errors.New("celeris/memcached: unexpected server reply with empty queue")
	}
	req := head.(*mcRequest)
	req.status = v.Kind
	switch v.Kind {
	case protocol.KindNumber:
		req.number = v.Int
	case protocol.KindVersion:
		req.version = string(v.Data)
	case protocol.KindError:
		req.resultErr = &MemcachedError{Kind: "ERROR"}
	case protocol.KindClientError:
		req.resultErr = &MemcachedError{Kind: "CLIENT_ERROR", Msg: string(v.Data)}
	case protocol.KindServerError:
		req.resultErr = &MemcachedError{Kind: "SERVER_ERROR", Msg: string(v.Data)}
	}
	req.finish()
	return nil
}

func (s *mcState) processBinary(data []byte) error {
	s.binR.Feed(data)
	for {
		pkt, err := s.binR.Next()
		if errors.Is(err, protocol.ErrIncomplete) {
			break
		}
		if err != nil {
			return err
		}
		if derr := s.dispatchBinary(pkt); derr != nil {
			return derr
		}
	}
	s.binR.Compact()
	return nil
}

// dispatchBinary routes one binary-protocol packet. Single-response ops
// (get/set/delete/incr/…) consume one packet and finish the head request;
// multi-response ops (GetMulti via OpGetKQ+OpNoop, Stats via OpStat) leave
// the head in place, accumulate packets, and only pop+finish on the
// terminator (as decided by req.binIsTerm).
func (s *mcState) dispatchBinary(p protocol.BinaryPacket) error {
	head := s.bridge.Head()
	if head == nil {
		return errors.New("celeris/memcached: unexpected server reply with empty queue")
	}
	req := head.(*mcRequest)
	if req.kind == kindBinaryMulti {
		return s.dispatchBinaryMulti(req, p)
	}
	s.bridge.Pop()
	req.binStatus = p.Status()
	if p.Status() == protocol.StatusOK {
		// GET-style replies carry the value in p.Value.
		if len(p.Value) > 0 {
			item := valueItem{
				Key:  string(p.Key),
				CAS:  p.Header.CAS,
				Data: copyBytes(p.Value),
			}
			// GET/GAT responses carry the 4-byte flags in extras.
			if len(p.Extras) >= 4 {
				item.Flags = binaryUint32(p.Extras[0:4])
			}
			req.values = append(req.values, item)
		} else if p.Header.CAS != 0 {
			// Storage replies leave Value empty but return the new CAS.
			req.number = p.Header.CAS
		}
	} else {
		req.resultErr = &MemcachedError{Status: p.Status(), Msg: string(p.Value)}
	}
	req.finish()
	return nil
}

// dispatchBinaryMulti routes one packet belonging to an in-flight
// kindBinaryMulti request. Non-terminator packets are appended to the
// request's accumulators (req.values for GetKQ hits, req.stats for STAT
// entries); the terminator pops the bridge and finishes the request. A
// non-OK status on an intermediate packet poisons the whole batch — the
// error propagates once the terminator arrives. A non-OK status on the
// terminator itself short-circuits and finishes immediately.
func (s *mcState) dispatchBinaryMulti(req *mcRequest, p protocol.BinaryPacket) error {
	terminator := req.binIsTerm != nil && req.binIsTerm(p)
	if !terminator {
		if p.Status() == protocol.StatusOK {
			switch p.Header.Opcode {
			case protocol.OpStat:
				req.stats = append(req.stats, statItem{
					Name:  string(p.Key),
					Value: string(p.Value),
				})
			default:
				// GetKQ / GetK style: key + extras(flags) + value.
				item := valueItem{
					Key:  string(p.Key),
					CAS:  p.Header.CAS,
					Data: copyBytes(p.Value),
				}
				if len(p.Extras) >= 4 {
					item.Flags = binaryUint32(p.Extras[0:4])
				}
				req.values = append(req.values, item)
			}
		} else {
			// First non-OK status wins; remember it but keep consuming until
			// the terminator arrives so we stay framing-aligned with the
			// server. A malformed key/server error is rare for these ops.
			if req.resultErr == nil {
				req.resultErr = &MemcachedError{Status: p.Status(), Msg: string(p.Value)}
			}
		}
		return nil
	}
	// Terminator: pop, record its own status (so callers can detect STAT
	// errors or Noop failures) and finish.
	if popped := s.bridge.Pop(); popped != req {
		return errors.New("celeris/memcached: bridge head mismatch on binary terminator")
	}
	req.binStatus = p.Status()
	if p.Status() != protocol.StatusOK && req.resultErr == nil {
		req.resultErr = &MemcachedError{Status: p.Status(), Msg: string(p.Value)}
	}
	req.finish()
	return nil
}

// binaryUint32 reads a big-endian uint32 from b.
func binaryUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// copyBytes returns a fresh copy of b (or nil if empty). Used to detach reply
// slices from the reader's internal buffer before signaling completion.
func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// drainWithError fails every pending request with err. The pendingGet /
// pendingStats pointers are purposely left alone — they are only touched
// from the worker goroutine (processRecv → dispatch), so nulling them
// here would race with an in-flight recv callback. The bridge drain
// below fails the matching requests; a subsequent worker-side dispatch
// that finds pendingGet set will operate on a finished request and
// discover the conn is closed on its next bridge.Pop.
func (s *mcState) drainWithError(err error) {
	s.bridge.DrainWithError(err, func(req async.PendingRequest, e error) {
		r := req.(*mcRequest)
		r.resultErr = e
		r.finish()
	})
}
