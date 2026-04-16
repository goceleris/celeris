package redis

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// errPipeCmdOrphaned is returned by typed cmd Result() when the owning
// Pipeline has been released back to the pool or was never properly exec'd.
var errPipeCmdOrphaned = &pipeCmd{err: ErrClosed}

// defaultPipelineCap is the initial capacity of Pipeline's per-command slabs.
// Chosen so a typical batch (tens to low-hundreds of commands) never grows
// mid-build. Pipelines that exceed the cap pay one slice-grow in the slab
// that holds the affected command kind — the slab only grows, and it is
// reused across [Client.Pipeline] calls through [pipelinePool] so the cost
// amortizes to zero in steady state.
const defaultPipelineCap = 128

// Pipeline batches commands into one wire write. Results are reachable via
// the *Cmd types returned from each method. A Pipeline is re-usable and
// pooled on its parent Client: allocate via [Client.Pipeline], read results
// after Exec, then return it to the pool via [Pipeline.Release]. Release is
// optional but recommended for tight hot paths — without it a fresh
// Pipeline (and its internal slabs) is allocated on every call.
//
// Pipeline is NOT safe for concurrent use. A caller must not invoke methods
// on the same Pipeline from multiple goroutines at once.
type Pipeline struct {
	client *Client
	// w accumulates wire bytes across AppendCommand calls.
	w *protocol.Writer

	// cmds owns the per-command tracking state by value. addCmd hands out
	// &cmds[i] pointers that are valid until the next Reset/Release — typed
	// cmd wrappers store the pointer and dereference it inside Result().
	// Capacity is grown once (on first overflow) and preserved across
	// Pipeline pool cycles.
	cmds []pipeCmd

	// Typed wrapper slabs. Each builder (Get/Set/Incr/...) grows the
	// matching slab, fills in its pc pointer, and returns &slab[len-1].
	// The slab's backing is stable across the Exec lifecycle — growth is
	// only triggered by the OWNING Pipeline's addCmd path, not by any
	// external mutation, and by the time Result() is called no further
	// append is in flight. When a Pipeline is Released and later reused,
	// the slabs are reset via [:0] so their backing (and any pointers to
	// it) is reassigned only by this Pipeline — no cross-Pipeline sharing.
	stringCmds []StringCmd
	intCmds    []IntCmd
	statusCmds []StatusCmd
	floatCmds  []FloatCmd
	boolCmds   []BoolCmd

	// reqs is reused across Exec calls to avoid allocating a fresh
	// []*redisRequest per call.
	reqs []*redisRequest

	// argsScratch backs variadic arg builds (HSet, LPush, ZAdd, ...). We
	// reuse it across addCmd calls within a single Exec; on Reset it is
	// truncated but its backing is retained.
	argsScratch []string

	// strSlab is a contiguous buffer holding all string results from the
	// direct-extraction pipeline path. unsafe.String references into this
	// slab are stored in pipeCmd.str. The slab is owned by the Pipeline
	// (not the conn), so strings remain valid until the next Exec/Discard/
	// Release — matching the documented lifetime of typed Cmd handles.
	strSlab []byte

	// reqSlab is a contiguous buffer of redisRequest objects reused across
	// Exec calls. Instead of 1000 individual sync.Pool round-trips for a
	// 1000-command pipeline, we allocate one slab and point reqs[i] into it.
	// Each entry's doneCh is allocated lazily on first use and retained for
	// subsequent Exec calls. The slab only grows and is never shrunk.
	reqSlab []redisRequest
}

// pipeCmd holds the per-command state populated by Exec. When direct is true,
// typed results live in the inline fields (str, scalar) — the val pointer is
// nil. When direct is false (async path, Tx, fallback), val points to a
// protocol.Value. The scalar field is a union: int64 for IntCmd, float64 bits
// for FloatCmd, 0/1 for BoolCmd. This layout keeps pipeCmd at 56 bytes
// instead of 184 bytes (with an embedded Value), saving ~128KB per
// 1000-command pipeline slab.
type pipeCmd struct {
	kind   cmdKind
	direct bool
	str    string
	scalar int64 // union: int64 | float64 bits (via math.Float64bits) | bool (0/1)
	err    error
	val    *protocol.Value
}

type cmdKind uint8

const (
	kindString cmdKind = iota
	kindInt
	kindStatus
	kindFloat
	kindBool
	kindArray
)

// kindToExpect maps a pipeline cmdKind to the dispatch-layer expectKind.
// Commands whose reply type is known a priori (string, int, status, float,
// bool) get a typed tag; unknown/array kinds fall back to expectNone which
// triggers the full Value copy path.
func kindToExpect(k cmdKind) expectKind {
	switch k {
	case kindString:
		return expectString
	case kindInt:
		return expectInt
	case kindStatus:
		return expectStatus
	case kindFloat:
		return expectFloat
	case kindBool:
		return expectBool
	default:
		return expectNone
	}
}

// pipelinePool recycles Pipelines (and their pre-grown internal slabs) so
// the common "per-request Pipeline" pattern amortizes to zero allocations
// after warmup.
var pipelinePool = sync.Pool{
	New: func() any {
		return &Pipeline{
			w:           protocol.NewWriterSize(4096),
			cmds:        make([]pipeCmd, 0, defaultPipelineCap),
			stringCmds:  make([]StringCmd, 0, defaultPipelineCap),
			intCmds:     make([]IntCmd, 0, defaultPipelineCap),
			statusCmds:  make([]StatusCmd, 0, defaultPipelineCap),
			floatCmds:   make([]FloatCmd, 0, defaultPipelineCap),
			boolCmds:    make([]BoolCmd, 0, defaultPipelineCap),
			reqs:        make([]*redisRequest, 0, defaultPipelineCap),
			argsScratch: make([]string, 0, 8),
			strSlab:     make([]byte, 0, 1024),
			reqSlab:     make([]redisRequest, 0, defaultPipelineCap),
		}
	},
}

// Pipeline returns a Pipeline bound to c. Call [Pipeline.Release] after
// reading results to return it to the pool for reuse — holding the
// Pipeline past Release invalidates every typed Cmd handle it produced.
func (c *Client) Pipeline() *Pipeline {
	p := pipelinePool.Get().(*Pipeline)
	p.client = c
	return p
}

// Release returns p to its Client's pool. After Release, every *StringCmd,
// *IntCmd, ... previously returned from p is invalid — reading from them is
// undefined behaviour. Safe to call at most once; subsequent calls are
// no-ops. Calling Release is optional; an un-Released Pipeline is simply
// GC'd like any other heap object.
func (p *Pipeline) Release() {
	if p == nil || p.client == nil {
		return
	}
	p.reset()
	// Release oversized slabs to the GC so a single spike (e.g. 100K
	// GETs returning 1KB each) does not permanently grow the pooled
	// Pipeline's memory footprint.
	if cap(p.strSlab) > maxSlabRetain {
		p.strSlab = nil
	}
	if cap(p.reqSlab)*int(unsafe.Sizeof(redisRequest{})) > maxSlabRetain {
		p.reqSlab = nil
	}
	p.client = nil
	pipelinePool.Put(p)
}

// reset clears queued commands while preserving slab capacity so the
// Pipeline can be handed to a new caller without re-allocating its
// internals.
func (p *Pipeline) reset() {
	p.w.Reset()
	// Zero used slots so previously-retained pointers (if any leaked) don't
	// observe stale values.
	for i := range p.cmds {
		p.cmds[i] = pipeCmd{}
	}
	p.cmds = p.cmds[:0]
	// Typed wrapper slabs: zero used slots for the same reason.
	for i := range p.stringCmds {
		p.stringCmds[i] = StringCmd{}
	}
	p.stringCmds = p.stringCmds[:0]
	for i := range p.intCmds {
		p.intCmds[i] = IntCmd{}
	}
	p.intCmds = p.intCmds[:0]
	for i := range p.statusCmds {
		p.statusCmds[i] = StatusCmd{}
	}
	p.statusCmds = p.statusCmds[:0]
	for i := range p.floatCmds {
		p.floatCmds[i] = FloatCmd{}
	}
	p.floatCmds = p.floatCmds[:0]
	for i := range p.boolCmds {
		p.boolCmds[i] = BoolCmd{}
	}
	p.boolCmds = p.boolCmds[:0]
	// Clear reqs refs; slab-backed requests are retained on p.reqSlab.
	for i := range p.reqs {
		p.reqs[i] = nil
	}
	p.reqs = p.reqs[:0]
	// Clear GC-relevant fields so stale ctx/result/error refs don't retain
	// heap objects across pool cycles. doneCh and scalar fields are preserved.
	for i := range p.reqSlab {
		p.reqSlab[i].ctx = nil
		p.reqSlab[i].result = protocol.Value{}
		p.reqSlab[i].resultErr = nil
		p.reqSlab[i].strResult = ""
	}
	p.reqSlab = p.reqSlab[:0]
	p.argsScratch = p.argsScratch[:0]
	p.strSlab = p.strSlab[:0]
}

// Discard drops all queued commands without sending anything. The Pipeline
// remains usable for subsequent enqueues.
func (p *Pipeline) Discard() {
	p.reset()
}

// Exec sends every queued command in one write and blocks until the last
// reply is received. Returns the first parse/transport error; per-command
// errors are reachable via each *Cmd.Result().
func (p *Pipeline) Exec(ctx context.Context) error {
	n := len(p.cmds)
	if n == 0 {
		return nil
	}
	conn, err := p.client.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	// Pre-size strSlab: typical GET response is ~16 bytes; SET response is
	// "OK" (interned, but slab growth may still occur for other cmds).
	// Over-sizing is cheap; under-sizing triggers grow-copies mid-pipeline.
	if estBytes := n * 16; cap(p.strSlab) < estBytes {
		p.strSlab = make([]byte, 0, estBytes)
	}
	// Ensure reqs has capacity for n; reuse backing.
	if cap(p.reqs) < n {
		p.reqs = make([]*redisRequest, n)
	} else {
		p.reqs = p.reqs[:n]
	}
	// Grow the request slab to n if needed. The slab is a contiguous buffer
	// of redisRequest objects that avoids n individual sync.Pool round-trips.
	// Each entry's doneCh is allocated lazily on first use and retained.
	freshSlab := false
	if cap(p.reqSlab) < n {
		p.reqSlab = make([]redisRequest, n)
		freshSlab = true
	} else {
		p.reqSlab = p.reqSlab[:n]
	}
	// Pre-populate requests with expected reply types so the sync pipeline
	// fast path in dispatch can extract typed results directly, bypassing
	// the intermediate protocol.Value copy.
	for i := 0; i < n; i++ {
		r := &p.reqSlab[i]
		r.ctx = ctx
		r.expect = kindToExpect(p.cmds[i].kind)
		if !freshSlab {
			// Reused slab: clear stale values. Fresh slabs are Go-zeroed.
			r.result = protocol.Value{}
			r.resultErr = nil
			r.owned = false
			r.isPush = false
			r.strResult = ""
			r.intResult = 0
			r.floatResult = 0
			r.boolResult = false
			r.finished.Store(false)
		}
		// Only the last request's doneCh is waited on (by execManyCore's
		// spin-wait and select). Intermediate requests are dispatched by
		// index (sync path) or via bridge (async fallback) — their finish()
		// is a no-op on nil doneCh thanks to the select+default in finish().
		// This saves N-1 channel allocations per pipeline batch.
		if i == n-1 {
			if r.doneCh == nil {
				r.doneCh = make(chan struct{}, 1)
			} else {
				select {
				case <-r.doneCh:
				default:
				}
			}
		}
		p.reqs[i] = r
	}
	// Engine Write() copies data into its per-FD outbound buffer before
	// returning, so we can hand the writer's internal slice directly without
	// allocating a copy. p.w is reset in p.reset() at Release/Discard time.
	reqs, err := conn.execManyIntoPrealloc(ctx, p.w.Bytes(), p.reqs)
	reader := conn.state.reader
	if err != nil {
		for i, r := range reqs {
			if r == nil || i >= n {
				continue
			}
			if r.resultErr != nil {
				p.cmds[i].err = r.resultErr
			} else {
				p.cmds[i].err = err
			}
			p.transferResult(&p.cmds[i], r)
			// Release pooled Reader slices but do NOT return slab-backed
			// requests to reqPool — they are owned by the Pipeline.
			if r.owned {
				reader.Release(r.result)
			}
			reqs[i] = nil
		}
		p.w.Reset()
		p.cmds = p.cmds[:0]
		p.reqs = p.reqs[:0]
		return err
	}
	var firstErr error
	for i, r := range reqs {
		p.transferResult(&p.cmds[i], r)
		if r.resultErr != nil {
			p.cmds[i].err = r.resultErr
			if firstErr == nil {
				firstErr = r.resultErr
			}
		}
		if r.owned {
			reader.Release(r.result)
		}
		reqs[i] = nil
	}
	// Shrink the conn's copySlab if a spike grew it beyond the retain
	// threshold. The slab was used during sync pipeline dispatch and all
	// results are now transferred into p.strSlab or pipeCmd.val.
	if cap(conn.state.copySlab) > maxSlabRetain {
		conn.state.copySlab = nil
	}
	p.client.pool.releaseCmd(conn)
	p.w.Reset()
	// Leave p.cmds populated so the user can read Result() off each typed
	// handle. The slabs are fully reset only on Release/Discard.
	p.reqs = p.reqs[:0]
	return firstErr
}

// transferResult moves the dispatch result from a redisRequest into a pipeCmd.
// When direct extraction was used, the typed result is copied into pipeCmd's
// inline fields and string data is re-homed into the Pipeline's strSlab.
// Otherwise a heap-allocated *Value is stored so the user can read it via
// the as*() helpers.
func (p *Pipeline) transferResult(pc *pipeCmd, r *redisRequest) {
	if r.expect != expectNone && r.resultErr == nil {
		pc.direct = true
		pc.str = p.slabStr(r.strResult)
		switch r.expect {
		case expectInt:
			pc.scalar = r.intResult
		case expectFloat:
			pc.scalar = int64(math.Float64bits(r.floatResult))
		case expectBool:
			if r.boolResult {
				pc.scalar = 1
			}
		}
	} else {
		// Allocate a Value on the heap. This path runs only for the async
		// fallback, unexpected reply types, or error replies — the common
		// sync pipeline case uses the direct path above.
		v := r.result
		pc.val = &v
	}
}

// slabStr appends the bytes of s into the Pipeline-owned strSlab and returns
// a string backed by the slab memory via unsafe.String. This avoids a
// per-string heap allocation — the entire pipeline's string results share one
// contiguous slab. The returned string is valid until reset/Release.
func (p *Pipeline) slabStr(s string) string {
	if len(s) == 0 {
		return ""
	}
	switch s {
	case "OK":
		return "OK"
	case "QUEUED":
		return "QUEUED"
	case "PONG":
		return "PONG"
	}
	off := len(p.strSlab)
	p.strSlab = append(p.strSlab, s...)
	return unsafe.String(&p.strSlab[off], len(s))
}

// addCmd appends args to the pipeline buffer and returns the index of the
// tracking pipeCmd. Callers must NOT hold a pointer into p.cmds across
// additional addCmd calls — the underlying array may reallocate.
func (p *Pipeline) addCmd(kind cmdKind, args ...string) int {
	p.w.AppendCommand(args...)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// addCmd2 is the 2-arg fast path that avoids the variadic []string alloc.
func (p *Pipeline) addCmd2(kind cmdKind, a0, a1 string) int {
	p.w.AppendCommand2(a0, a1)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// addCmd3 is the 3-arg fast path.
func (p *Pipeline) addCmd3(kind cmdKind, a0, a1, a2 string) int {
	p.w.AppendCommand3(a0, a1, a2)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// addCmd4 is the 4-arg fast path.
func (p *Pipeline) addCmd4(kind cmdKind, a0, a1, a2, a3 string) int {
	p.w.AppendCommand4(a0, a1, a2, a3)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// ---------- Deferred result handles ----------
//
// Each typed Cmd stores a back-pointer to the owning Pipeline and the index
// into p.cmds. Result() resolves &p.cmds[idx] at call time — by then Exec
// has completed and p.cmds is frozen. This avoids dangling pointers when the
// cmds slice grows past its initial capacity during command building.
//
// For Tx (which allocates each pipeCmd individually), the pc field is set
// directly and p is nil. Result() checks pc first.

// StringCmd is a deferred string reply.
type StringCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *StringCmd) cmd() *pipeCmd {
	if c.pc != nil {
		return c.pc
	}
	if c.p == nil || c.idx >= len(c.p.cmds) {
		return errPipeCmdOrphaned
	}
	return &c.p.cmds[c.idx]
}

// Result returns the decoded string and error.
func (c *StringCmd) Result() (string, error) {
	pc := c.cmd()
	if pc.err != nil {
		return "", pc.err
	}
	if pc.direct {
		return pc.str, nil
	}
	return asString(*pc.val)
}

// IntCmd is a deferred int64 reply.
type IntCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *IntCmd) cmd() *pipeCmd {
	if c.pc != nil {
		return c.pc
	}
	if c.p == nil || c.idx >= len(c.p.cmds) {
		return errPipeCmdOrphaned
	}
	return &c.p.cmds[c.idx]
}

// Result returns the int64 and error.
func (c *IntCmd) Result() (int64, error) {
	pc := c.cmd()
	if pc.err != nil {
		return 0, pc.err
	}
	if pc.direct {
		return pc.scalar, nil
	}
	return asInt(*pc.val)
}

// StatusCmd is a deferred simple-string reply.
type StatusCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *StatusCmd) cmd() *pipeCmd {
	if c.pc != nil {
		return c.pc
	}
	if c.p == nil || c.idx >= len(c.p.cmds) {
		return errPipeCmdOrphaned
	}
	return &c.p.cmds[c.idx]
}

// Result returns the status string and error.
func (c *StatusCmd) Result() (string, error) {
	pc := c.cmd()
	if pc.err != nil {
		return "", pc.err
	}
	if pc.direct {
		return pc.str, nil
	}
	return asString(*pc.val)
}

// FloatCmd is a deferred float reply.
type FloatCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *FloatCmd) cmd() *pipeCmd {
	if c.pc != nil {
		return c.pc
	}
	if c.p == nil || c.idx >= len(c.p.cmds) {
		return errPipeCmdOrphaned
	}
	return &c.p.cmds[c.idx]
}

// Result returns the float64 and error.
func (c *FloatCmd) Result() (float64, error) {
	pc := c.cmd()
	if pc.err != nil {
		return 0, pc.err
	}
	if pc.direct {
		return math.Float64frombits(uint64(pc.scalar)), nil
	}
	return asFloat(*pc.val)
}

// BoolCmd is a deferred bool reply.
type BoolCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *BoolCmd) cmd() *pipeCmd {
	if c.pc != nil {
		return c.pc
	}
	if c.p == nil || c.idx >= len(c.p.cmds) {
		return errPipeCmdOrphaned
	}
	return &c.p.cmds[c.idx]
}

// Result returns the bool and error.
func (c *BoolCmd) Result() (bool, error) {
	pc := c.cmd()
	if pc.err != nil {
		return false, pc.err
	}
	if pc.direct {
		return pc.scalar != 0, nil
	}
	return asBool(*pc.val)
}

// ---------- Slab-allocating typed-cmd helpers ----------

func (p *Pipeline) newIntCmd(idx int) *IntCmd {
	p.intCmds = append(p.intCmds, IntCmd{p: p, idx: idx})
	return &p.intCmds[len(p.intCmds)-1]
}

func (p *Pipeline) newStringCmd(idx int) *StringCmd {
	p.stringCmds = append(p.stringCmds, StringCmd{p: p, idx: idx})
	return &p.stringCmds[len(p.stringCmds)-1]
}

func (p *Pipeline) newStatusCmd(idx int) *StatusCmd {
	p.statusCmds = append(p.statusCmds, StatusCmd{p: p, idx: idx})
	return &p.statusCmds[len(p.statusCmds)-1]
}

func (p *Pipeline) newBoolCmd(idx int) *BoolCmd {
	p.boolCmds = append(p.boolCmds, BoolCmd{p: p, idx: idx})
	return &p.boolCmds[len(p.boolCmds)-1]
}

// ---------- Pipeline commands ----------

// Get enqueues GET.
func (p *Pipeline) Get(key string) *StringCmd {
	return p.newStringCmd(p.addCmd2(kindString, "GET", key))
}

// Set enqueues SET with optional EX/PX.
func (p *Pipeline) Set(key string, value any, expiration time.Duration) *StatusCmd {
	if expiration <= 0 {
		return p.newStatusCmd(p.addCmd3(kindStatus, "SET", key, argify(value)))
	}
	p.argsScratch = append(p.argsScratch[:0], "SET", key, argify(value))
	p.argsScratch = appendExpire(p.argsScratch, expiration)
	return p.newStatusCmd(p.addCmd(kindStatus, p.argsScratch...))
}

// Del enqueues DEL.
func (p *Pipeline) Del(keys ...string) *IntCmd {
	if len(keys) == 1 {
		return p.newIntCmd(p.addCmd2(kindInt, "DEL", keys[0]))
	}
	p.argsScratch = append(p.argsScratch[:0], "DEL")
	p.argsScratch = append(p.argsScratch, keys...)
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// Incr enqueues INCR.
func (p *Pipeline) Incr(key string) *IntCmd {
	return p.newIntCmd(p.addCmd2(kindInt, "INCR", key))
}

// Decr enqueues DECR.
func (p *Pipeline) Decr(key string) *IntCmd {
	return p.newIntCmd(p.addCmd2(kindInt, "DECR", key))
}

// Exists enqueues EXISTS.
func (p *Pipeline) Exists(keys ...string) *IntCmd {
	if len(keys) == 1 {
		return p.newIntCmd(p.addCmd2(kindInt, "EXISTS", keys[0]))
	}
	p.argsScratch = append(p.argsScratch[:0], "EXISTS")
	p.argsScratch = append(p.argsScratch, keys...)
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// HGet enqueues HGET.
func (p *Pipeline) HGet(key, field string) *StringCmd {
	return p.newStringCmd(p.addCmd3(kindString, "HGET", key, field))
}

// HSet enqueues HSET.
func (p *Pipeline) HSet(key string, values ...any) *IntCmd {
	p.argsScratch = append(p.argsScratch[:0], "HSET", key)
	for _, v := range values {
		p.argsScratch = append(p.argsScratch, argify(v))
	}
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// LPush enqueues LPUSH.
func (p *Pipeline) LPush(key string, values ...any) *IntCmd {
	p.argsScratch = append(p.argsScratch[:0], "LPUSH", key)
	for _, v := range values {
		p.argsScratch = append(p.argsScratch, argify(v))
	}
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// RPush enqueues RPUSH.
func (p *Pipeline) RPush(key string, values ...any) *IntCmd {
	p.argsScratch = append(p.argsScratch[:0], "RPUSH", key)
	for _, v := range values {
		p.argsScratch = append(p.argsScratch, argify(v))
	}
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// SAdd enqueues SADD.
func (p *Pipeline) SAdd(key string, members ...any) *IntCmd {
	p.argsScratch = append(p.argsScratch[:0], "SADD", key)
	for _, m := range members {
		p.argsScratch = append(p.argsScratch, argify(m))
	}
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// ZAdd enqueues ZADD.
func (p *Pipeline) ZAdd(key string, members ...Z) *IntCmd {
	p.argsScratch = append(p.argsScratch[:0], "ZADD", key)
	for _, m := range members {
		p.argsScratch = append(p.argsScratch, strconv.FormatFloat(m.Score, 'f', -1, 64), argify(m.Member))
	}
	return p.newIntCmd(p.addCmd(kindInt, p.argsScratch...))
}

// Expire enqueues EXPIRE.
func (p *Pipeline) Expire(key string, d time.Duration) *BoolCmd {
	secs := int64(d / time.Second)
	if secs <= 0 {
		secs = 1
	}
	return p.newBoolCmd(p.addCmd3(kindBool, "EXPIRE", key, strconv.FormatInt(secs, 10)))
}
