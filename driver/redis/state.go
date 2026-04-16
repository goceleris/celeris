package redis

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/redis/protocol"
)

// mode tags the current protocol dialect on a single conn.
type mode uint32

const (
	modeCmd mode = iota
	modePubSub
)

// reqPool recycles redisRequest values — allocating one per command was a
// top contributor to GET/SET allocation budgets before v1.4.0. doneCh is a
// buffered channel with capacity 1; finish() sends into it (instead of
// closing it) so the channel itself can be reused across pool cycles without
// reallocation. Exactly one waiter receives per request. Callers MUST NOT
// retain a request reference after releaseResult / returning it to the pool.
var reqPool = sync.Pool{
	New: func() any {
		return &redisRequest{doneCh: make(chan struct{}, 1)}
	},
}

// getRequest returns a zeroed redisRequest with a drained, reusable doneCh.
func getRequest(ctx context.Context) *redisRequest {
	r := reqPool.Get().(*redisRequest)
	r.ctx = ctx
	r.result = protocol.Value{}
	r.resultErr = nil
	r.owned = false
	r.isPush = false
	r.expect = expectNone
	r.strResult = ""
	r.intResult = 0
	r.floatResult = 0
	r.boolResult = false
	r.finished.Store(false)
	// doneCh was drained by the wait path (or putRequest as a safety net)
	// before recycling; no action needed here.
	return r
}

// putRequest returns r to the pool. The doneCh on r is buffered (cap=1); if
// finish() sent an item and no waiter received it (e.g. ctx-cancel path
// races), drain it here so the next acquirer sees an empty channel.
func putRequest(r *redisRequest) {
	if r == nil {
		return
	}
	select {
	case <-r.doneCh:
	default:
	}
	r.ctx = nil
	reqPool.Put(r)
}

// expectKind tags the expected reply type for a pipeline command. When set on
// a redisRequest used in the sync pipeline fast path, dispatch extracts the
// typed result directly instead of copying the full protocol.Value — saving
// the ~120-byte Value allocation per response.
type expectKind uint8

const (
	expectNone   expectKind = iota // fallback: store full Value
	expectString                   // bulk/simple → strResult
	expectInt                      // integer → intResult
	expectStatus                   // simple string → strResult
	expectFloat                    // double → floatResult
	expectBool                     // bool/int → boolResult
)

// redisRequest is one in-flight command awaiting a reply. Result pointers
// are populated on the worker goroutine; the handler goroutine waits on
// doneCh.
type redisRequest struct {
	ctx    context.Context
	result protocol.Value
	// resultErr carries transport or server error. A server error reply is
	// decoded and returned as *RedisError via resultErr; successful replies
	// leave it nil.
	resultErr error
	// owned is true when result's aggregate slices came from the Reader's
	// pool and must be released after the handler copies out.
	owned  bool
	doneCh chan struct{}
	// finished guards doneCh closure so dispatch, drainWithError, and any
	// other completer cannot race to close it (otherwise concurrent
	// completers would panic on the second close). Using atomic.Bool
	// instead of sync.Once avoids a reset race when the request is
	// recycled via reqPool.
	finished atomic.Bool
	// dedicated flags prevent Release-to-pool for special frames.
	isPush bool

	// Typed result fields for sync pipeline fast path. When expect != expectNone,
	// dispatch stores the result directly here instead of in result (Value).
	expect      expectKind
	strResult   string
	intResult   int64
	floatResult float64
	boolResult  bool
}

// finish signals the request as complete exactly once. Safe to call from
// multiple goroutines. doneCh is buffered (cap=1); the nonblocking send wakes
// the (single) waiter without allocating.
func (r *redisRequest) finish() {
	if r.finished.CompareAndSwap(false, true) {
		select {
		case r.doneCh <- struct{}{}:
		default:
		}
	}
}

// Ctx implements async.PendingRequest.
func (r *redisRequest) Ctx() context.Context { return r.ctx }

// pubsubRouter dispatches push frames to a PubSub's message channel.
type pubsubRouter struct {
	mu     sync.Mutex
	target *PubSub
}

func (r *pubsubRouter) set(p *PubSub) {
	r.mu.Lock()
	r.target = p
	r.mu.Unlock()
}

func (r *pubsubRouter) clear() {
	r.mu.Lock()
	r.target = nil
	r.mu.Unlock()
}

//nolint:unparam // bool return kept for test introspection and backpressure
func (r *pubsubRouter) deliver(m *Message) bool {
	r.mu.Lock()
	t := r.target
	r.mu.Unlock()
	if t == nil {
		return false
	}
	return t.deliver(m)
}

// redisState is the event-loop-side decoder shared by a single redisConn. It
// feeds bytes into a protocol.Reader and hands off complete frames either to
// the head of the pending queue (cmd / txn modes) or to the pubsub router.
type redisState struct {
	reader *protocol.Reader
	writer *protocol.Writer
	bridge *async.Bridge
	mode   atomic.Uint32 // of type mode
	proto  atomic.Int32  // 2 or 3
	router *pubsubRouter

	// onPush is called when a RESP3 push frame arrives on a modeCmd
	// connection. Access is not mutex-guarded because the field is set
	// once at dial time or via Client.OnPush before traffic starts, and
	// the callback itself must be safe for concurrent invocation.
	onPush func(channel string, data []protocol.Value)

	// syncPipe fields enable a zero-bridge fast path for pipelined responses
	// processed synchronously on the caller goroutine (via WriteAndPollMulti).
	// When syncPipeReqs is non-nil, dispatch routes responses directly into
	// the request slice by index instead of popping from the bridge queue,
	// eliminating 2N mutex operations for an N-command pipeline. The fields
	// are cleared in the beforeRearm callback (under recvMu) so the event
	// loop never observes them.
	syncPipeReqs []*redisRequest
	syncPipeIdx  int

	// copySlab is a single contiguous buffer used by copyValueSlab to batch
	// all string copies during a sync pipeline dispatch. Instead of one
	// allocation per response, all Str bytes are appended to this slab and
	// sub-sliced. The slab is reset ([:0]) at the start of each pipeline
	// Exec but the backing array is retained across calls, so steady-state
	// pipelines of a given size never allocate. Slabs exceeding
	// maxSlabRetain after a dispatch cycle are released to the GC.
	copySlab []byte
}

// maxSlabRetain is the high-water mark above which slabs are released to the
// GC after a dispatch cycle. A single spike (e.g. 100K GETs returning 1KB
// each) permanently grew the slab to 100MB without this check.
const maxSlabRetain = 1 << 20 // 1 MiB

func newRedisState() *redisState {
	return &redisState{
		reader: protocol.NewReader(),
		writer: protocol.NewWriter(),
		bridge: async.NewBridge(),
		router: &pubsubRouter{},
	}
}

// setMode atomically changes the mode.
func (s *redisState) setMode(m mode) {
	s.mode.Store(uint32(m))
}

// currentMode returns the current mode.
func (s *redisState) currentMode() mode {
	return mode(s.mode.Load())
}

// processRedis feeds incoming bytes and drains complete frames. Returns a
// non-nil error on protocol violation; the caller closes the conn.
//
// It runs on the worker goroutine and MUST NOT block.
func (s *redisState) processRedis(data []byte) error {
	s.reader.Feed(data)
	for {
		v, err := s.reader.Next()
		if errors.Is(err, protocol.ErrIncomplete) {
			break
		}
		if err != nil {
			return err
		}
		if derr := s.dispatch(v); derr != nil {
			return derr
		}
	}
	s.reader.Compact()
	return nil
}

// dispatch routes one parsed value to the appropriate consumer.
func (s *redisState) dispatch(v protocol.Value) error {
	// Skip RESP3 attribute frames — they are metadata prefacing the next real
	// reply.
	if v.Type == protocol.TyAttr {
		s.reader.Release(v)
		return nil
	}
	// Push frames are pubsub messages or out-of-band notifications.
	if v.Type == protocol.TyPush {
		if s.currentMode() == modeCmd {
			s.routeCmdPush(v)
		} else {
			s.routePush(v)
		}
		return nil
	}
	// In pubsub mode RESP2 dialects deliver messages as 3-elem arrays
	// starting with "message" or "pmessage". Also control replies land
	// here (subscribe/unsubscribe).
	if s.currentMode() == modePubSub && v.Type == protocol.TyArray {
		if isPubSubArray(v) {
			s.routePush(v)
			return nil
		}
	}

	// Sync pipeline fast path: dispatch directly by index, skip bridge.
	// For intermediate (non-last) requests we set finished atomically but
	// skip the channel send — nobody is waiting on individual doneCh in
	// the sync path. Only the last request gets a full finish() so isDone
	// fires immediately.
	var req *redisRequest
	syncPipe := s.syncPipeReqs != nil
	if syncPipe {
		idx := s.syncPipeIdx
		if idx >= len(s.syncPipeReqs) {
			s.reader.Release(v)
			return errors.New("celeris/redis: sync pipeline overflow")
		}
		req = s.syncPipeReqs[idx]
		s.syncPipeIdx = idx + 1
	} else {
		head := s.bridge.Pop()
		if head == nil {
			if s.currentMode() == modePubSub {
				s.reader.Release(v)
				return nil
			}
			s.reader.Release(v)
			return errors.New("celeris/redis: unexpected server reply with empty queue")
		}
		req = head.(*redisRequest)
	}
	if v.Type == protocol.TyError || v.Type == protocol.TyBlobErr {
		req.resultErr = parseError(v.Str)
		req.expect = expectNone
		s.reader.Release(v)
	} else if syncPipe && req.expect != expectNone {
		// Direct-extraction fast path: extract the typed result without
		// materializing a full protocol.Value copy. For scalar types (int,
		// bool, float) this is zero-alloc. For string types only the final
		// Go string is allocated (via slab), skipping the 120-byte Value.
		s.extractDirect(req, v)
		s.reader.Release(v)
	} else {
		// Detach the reply from the reader's internal buffer so the request
		// remains valid across subsequent Feed/Compact cycles. Without this
		// copy, a later chunk arriving on this conn can overwrite the
		// backing array that an earlier reply's Str slice aliases — which
		// manifests in pipelines as a reply that appears to carry the
		// bytes of a later reply (e.g. "2\r" from ":2\r\n" overwriting
		// "+OK\r\n").
		//
		// For sync pipeline dispatch, use the slab-based copy which
		// amortizes all string copies into one contiguous buffer.
		if syncPipe {
			req.result = s.copyValueSlab(v)
		} else {
			req.result = copyValueDetached(v)
		}
		s.reader.Release(v)
		req.owned = false
		// Clear expect so the Exec copy loop knows a full Value was stored
		// (not a direct-extracted typed result).
		req.expect = expectNone
	}
	if syncPipe {
		// Intermediate requests never need a signal — the caller reads their
		// results by index after the last req completes, and nobody waits on
		// their doneCh. For the LAST request we MUST use finish() (not just
		// finished.Store), because execManyCore falls through to spin-wait on
		// reqs[n-1].doneCh whenever WriteAndPollMulti returns ok=false. That
		// can legitimately happen if data arrives in the tiny window between
		// the final drain's EAGAIN and the EPOLLIN re-arm — the last response
		// then lands via bridge dispatch, but only if reqs[n-1] was still in
		// syncPipeReqs at dispatch time. If it was dispatched via sync pipe,
		// bridge never sees it and only the doneCh signal wakes the caller.
		if s.syncPipeIdx >= len(s.syncPipeReqs) {
			req.finish()
		}
	} else {
		req.finish()
	}
	return nil
}

// extractDirect extracts the typed result from v directly into req's typed
// fields, bypassing the intermediate protocol.Value copy entirely. String
// bytes are appended to copySlab and converted to a Go string via
// unsafe.String, amortizing all string copies into one contiguous buffer
// (the slab) with zero per-string allocation. Scalars (int, float, bool)
// are zero-alloc. The strings remain valid as long as the copySlab is not
// reclaimed, which is guaranteed through the Pipeline.Exec lifecycle.
func (s *redisState) extractDirect(req *redisRequest, v protocol.Value) {
	switch req.expect {
	case expectString:
		switch v.Type {
		case protocol.TyBulk, protocol.TySimple:
			req.strResult = s.slabString(v.Str)
		case protocol.TyVerbatim:
			b := v.Str
			if len(b) >= 4 && b[3] == ':' {
				b = b[4:]
			}
			req.strResult = s.slabString(b)
		case protocol.TyNull:
			req.resultErr = ErrNil
		case protocol.TyInt:
			req.strResult = strconv.FormatInt(v.Int, 10)
		default:
			req.result = s.copyValueSlab(v)
			req.expect = expectNone
		}
	case expectStatus:
		switch v.Type {
		case protocol.TySimple, protocol.TyBulk:
			req.strResult = s.slabString(v.Str)
		case protocol.TyNull:
			req.resultErr = ErrNil
		default:
			req.result = s.copyValueSlab(v)
			req.expect = expectNone
		}
	case expectInt:
		switch v.Type {
		case protocol.TyInt:
			req.intResult = v.Int
		case protocol.TyBulk, protocol.TySimple:
			n, err := strconv.ParseInt(string(v.Str), 10, 64)
			if err != nil {
				req.result = s.copyValueSlab(v)
				req.expect = expectNone
			} else {
				req.intResult = n
			}
		case protocol.TyNull:
			req.resultErr = ErrNil
		default:
			req.result = s.copyValueSlab(v)
			req.expect = expectNone
		}
	case expectFloat:
		switch v.Type {
		case protocol.TyDouble:
			req.floatResult = v.Float
		case protocol.TyBulk, protocol.TySimple:
			f, err := strconv.ParseFloat(string(v.Str), 64)
			if err != nil {
				req.result = s.copyValueSlab(v)
				req.expect = expectNone
			} else {
				req.floatResult = f
			}
		case protocol.TyNull:
			req.resultErr = ErrNil
		default:
			req.result = s.copyValueSlab(v)
			req.expect = expectNone
		}
	case expectBool:
		switch v.Type {
		case protocol.TyBool:
			req.boolResult = v.Bool
		case protocol.TyInt:
			req.boolResult = v.Int != 0
		case protocol.TyNull:
			req.resultErr = ErrNil
		default:
			req.result = s.copyValueSlab(v)
			req.expect = expectNone
		}
	default:
		req.result = s.copyValueSlab(v)
	}
}

// Interned strings for common Redis responses. Checking this table before
// falling through to slabString avoids a slab-copy + unsafe.String for the
// most frequent replies (e.g. +OK on every SET).
var internedStrings = [...]struct {
	b []byte
	s string
}{
	{[]byte("OK"), "OK"},
	{[]byte("QUEUED"), "QUEUED"},
	{[]byte("PONG"), "PONG"},
}

// slabString appends b to copySlab and returns a Go string backed by the
// slab memory via unsafe.String. This avoids per-string heap allocation —
// the entire pipeline's string data lives in one contiguous slab. The
// returned string is valid for the lifetime of the slab (reset at the
// start of each Exec).
func (s *redisState) slabString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	for i := range internedStrings {
		if bytes.Equal(b, internedStrings[i].b) {
			return internedStrings[i].s
		}
	}
	off := len(s.copySlab)
	s.copySlab = append(s.copySlab, b...)
	return unsafe.String(&s.copySlab[off], len(b))
}

// copyValueSlab deep-copies v's string fields into the redisState's copySlab
// buffer, amortizing allocation across an entire pipeline batch. All Str bytes
// are appended to slab and sub-sliced, so an N-command pipeline with short
// replies costs one grow (if any) instead of N allocations.
func (s *redisState) copyValueSlab(v protocol.Value) protocol.Value {
	switch v.Type {
	case protocol.TyInt, protocol.TyBool, protocol.TyDouble, protocol.TyNull:
		return v
	case protocol.TySimple, protocol.TyError, protocol.TyBulk, protocol.TyVerbatim, protocol.TyBlobErr:
		out := v
		if len(v.Str) > 0 {
			off := len(s.copySlab)
			s.copySlab = append(s.copySlab, v.Str...)
			out.Str = s.copySlab[off:len(s.copySlab):len(s.copySlab)]
		}
		protocol.ClearPooledFlags(&out)
		return out
	case protocol.TyBigInt:
		out := v
		if len(v.BigN) > 0 {
			off := len(s.copySlab)
			s.copySlab = append(s.copySlab, v.BigN...)
			out.BigN = s.copySlab[off:len(s.copySlab):len(s.copySlab)]
		}
		protocol.ClearPooledFlags(&out)
		return out
	default:
		out := v
		if len(v.Str) > 0 {
			off := len(s.copySlab)
			s.copySlab = append(s.copySlab, v.Str...)
			out.Str = s.copySlab[off:len(s.copySlab):len(s.copySlab)]
		}
		if len(v.BigN) > 0 {
			off := len(s.copySlab)
			s.copySlab = append(s.copySlab, v.BigN...)
			out.BigN = s.copySlab[off:len(s.copySlab):len(s.copySlab)]
		}
		if v.Array != nil {
			arr := make([]protocol.Value, len(v.Array))
			for i, e := range v.Array {
				arr[i] = s.copyValueSlab(e)
			}
			out.Array = arr
		}
		if v.Map != nil {
			m := make([]protocol.KV, len(v.Map))
			for i, kv := range v.Map {
				m[i] = protocol.KV{K: s.copyValueSlab(kv.K), V: s.copyValueSlab(kv.V)}
			}
			out.Map = m
		}
		protocol.ClearPooledFlags(&out)
		return out
	}
}

// copyValueDetached deep-copies v into freshly allocated buffers so the result
// no longer aliases the Reader's internal buffer or pooled aggregate slices.
// The returned Value is safe to retain past the next Feed/Next/Release cycle
// and MUST NOT be re-released into the reader's pool.
//
// Scalar types (Int, Bool, Double, Null) carry no slice aliases and are
// returned as-is (zero-copy). String-bearing types only allocate when the
// source Str aliases the reader buffer.
func copyValueDetached(v protocol.Value) protocol.Value {
	switch v.Type {
	case protocol.TyInt, protocol.TyBool, protocol.TyDouble, protocol.TyNull:
		return v
	case protocol.TySimple, protocol.TyError, protocol.TyBulk, protocol.TyVerbatim, protocol.TyBlobErr:
		out := v
		if len(v.Str) > 0 {
			s := make([]byte, len(v.Str))
			copy(s, v.Str)
			out.Str = s
		}
		protocol.ClearPooledFlags(&out)
		return out
	case protocol.TyBigInt:
		out := v
		if len(v.BigN) > 0 {
			b := make([]byte, len(v.BigN))
			copy(b, v.BigN)
			out.BigN = b
		}
		protocol.ClearPooledFlags(&out)
		return out
	default:
		// Aggregate types: Array, Set, Push, Map, Attr.
		out := v
		if len(v.Str) > 0 {
			s := make([]byte, len(v.Str))
			copy(s, v.Str)
			out.Str = s
		}
		if len(v.BigN) > 0 {
			b := make([]byte, len(v.BigN))
			copy(b, v.BigN)
			out.BigN = b
		}
		if v.Array != nil {
			arr := make([]protocol.Value, len(v.Array))
			for i, e := range v.Array {
				arr[i] = copyValueDetached(e)
			}
			out.Array = arr
		}
		if v.Map != nil {
			m := make([]protocol.KV, len(v.Map))
			for i, kv := range v.Map {
				m[i] = protocol.KV{K: copyValueDetached(kv.K), V: copyValueDetached(kv.V)}
			}
			out.Map = m
		}
		protocol.ClearPooledFlags(&out)
		return out
	}
}

// routeCmdPush handles a RESP3 push frame received on a modeCmd connection.
// If onPush is registered it is invoked with the push channel (first element)
// and the remaining data elements deep-copied so the callback can retain them
// past the next read cycle. If no callback is registered the frame is dropped.
func (s *redisState) routeCmdPush(v protocol.Value) {
	defer s.reader.Release(v)
	if s.onPush == nil || len(v.Array) == 0 {
		return
	}
	channel := string(v.Array[0].Str)
	var data []protocol.Value
	if len(v.Array) > 1 {
		data = make([]protocol.Value, len(v.Array)-1)
		for i, e := range v.Array[1:] {
			data[i] = copyValueDetached(e)
		}
	}
	s.onPush(channel, data)
}

// routePush turns a push / pubsub array into a Message and dispatches.
func (s *redisState) routePush(v protocol.Value) {
	defer s.reader.Release(v)
	if len(v.Array) < 3 {
		return
	}
	kind := string(v.Array[0].Str)
	switch kind {
	case "message":
		if len(v.Array) >= 3 {
			ch := string(v.Array[1].Str)
			payload := string(v.Array[2].Str)
			s.router.deliver(&Message{Channel: ch, Payload: payload})
		}
	case "pmessage":
		if len(v.Array) >= 4 {
			pat := string(v.Array[1].Str)
			ch := string(v.Array[2].Str)
			payload := string(v.Array[3].Str)
			s.router.deliver(&Message{Pattern: pat, Channel: ch, Payload: payload})
		}
	case "smessage":
		if len(v.Array) >= 3 {
			ch := string(v.Array[1].Str)
			payload := string(v.Array[2].Str)
			s.router.deliver(&Message{Channel: ch, Payload: payload, Shard: true})
		}
	case "subscribe", "unsubscribe", "psubscribe", "punsubscribe",
		"ssubscribe", "sunsubscribe":
		// Control acks — drop silently; PubSub tracks subs independently.
	}
}

// isPubSubArray returns true when a RESP2 array looks like a pubsub message
// or control reply ("message", "pmessage", "subscribe", "unsubscribe",
// "psubscribe", "punsubscribe").
func isPubSubArray(v protocol.Value) bool {
	if len(v.Array) < 3 {
		return false
	}
	first := v.Array[0]
	if first.Type != protocol.TyBulk && first.Type != protocol.TySimple {
		return false
	}
	switch string(first.Str) {
	case "message", "pmessage", "smessage":
		return true
	case "subscribe", "unsubscribe", "psubscribe", "punsubscribe",
		"ssubscribe", "sunsubscribe":
		return true
	}
	return false
}

// drainWithError fails every pending request with err.
func (s *redisState) drainWithError(err error) {
	s.bridge.DrainWithError(err, func(req async.PendingRequest, e error) {
		r := req.(*redisRequest)
		r.resultErr = e
		r.finish()
	})
}
