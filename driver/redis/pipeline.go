package redis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/internal/driver/redis/protocol"
)

// errPipeCmdOrphaned is returned by typed cmd Result() when the owning
// Pipeline has been released back to the pool or was never properly exec'd.
var errPipeCmdOrphaned = &pipeCmd{err: errPipelineReleased}

// errPipelineReleased is returned for any use of a [Pipeline] after
// [Pipeline.Release]: queued commands are not sent, Exec fails, and the typed
// handles report it. It wraps [ErrClosed].
var errPipelineReleased = fmt.Errorf("celeris-redis: Pipeline already released: %w", ErrClosed)

// errCmdNotExecuted is returned by a typed Cmd's Result() when the command
// has no reply to report: its Pipeline or Tx was never executed, or the Exec
// failed before the command was sent.
var errCmdNotExecuted = errors.New("celeris-redis: command has no result: its Pipeline or Tx was not executed")

// errPipelineExecuted is returned by a second [Pipeline.Exec] on a Pipeline
// that already ran. The commands were sent once and their results are held
// until [Pipeline.Discard] or [Pipeline.Release].
var errPipelineExecuted = errors.New("celeris-redis: Pipeline already executed; call Discard before queueing a new batch")

// defaultPipelineCap is the initial capacity of the pooled per-command buffers
// (the command list and the request slab). Chosen so a typical batch (tens to
// low-hundreds of commands) never grows them mid-build. They only grow, and
// are reused across [Client.Pipeline] calls through [pipelinePool].
const defaultPipelineCap = 128

// pipelineInlineCmds is how many typed Cmd handles a Pipeline carries inside
// its own allocation. A batch that queues more takes a larger slab once.
const pipelineInlineCmds = 16

// pipelineBigCmds is the handle capacity of the larger Pipeline allocation,
// used when the pooled state last carried a batch above the inline size and
// up to this many commands: such a Pipeline still costs one allocation.
const pipelineBigCmds = 128

// pipelineOverflowCmds is the size of the first handle slab beyond the inline
// ones when the pooled state has no batch size to go by; later slabs double.
const pipelineOverflowCmds = 128

// Pipeline batches commands into one wire write. Results are reachable via
// the *Cmd types returned from each method. Allocate one via
// [Client.Pipeline], queue commands, call [Pipeline.Exec] once, read the
// results, then return its buffers to the pool via [Pipeline.Release].
// Release is optional but recommended for tight hot paths: without it the
// buffers are simply garbage collected and the next Pipeline allocates new
// ones.
//
// Values obtained from a Pipeline are valid until Release: the strings and
// the typed results read from its *Cmd handles share the Pipeline's recycled
// buffers, and Release hands those buffers to the next Pipeline. Copy what you
// keep past Release (for example with strings.Clone).
//
// The Pipeline value itself is never recycled: after Release every method on
// it, and every *Cmd it produced, reports an error (queued commands are not
// sent, [Pipeline.Exec] fails, Result returns an error) and can never reach a
// Pipeline another caller holds. Release is idempotent.
//
// A Pipeline runs once: queue commands, call [Pipeline.Exec] once, read the
// results, then [Pipeline.Release] (or [Pipeline.Discard] to queue a new
// batch). A second Exec after a successful one returns an error. After an
// Exec that failed, each Result returns that error, and queueing commands
// starts a new batch that the next Exec sends. A Result read before Exec
// returns an error.
//
// Pipeline is NOT safe for concurrent use. A caller must not invoke methods
// on the same Pipeline from multiple goroutines at once.
type Pipeline struct {
	client *Client
	// st holds the pooled buffers; nil once released.
	st atomic.Pointer[pipeState]
	// refs backs the typed Cmd handles handed out by the builders. It lives
	// in the (never recycled) Pipeline, not in the pooled state, so a handle
	// kept past Release cannot be overwritten by another caller's Pipeline.
	// Handles are immutable once written, so a grown slab leaves the handles
	// pointing into the old one valid.
	refs []cmdRef
	// hint is the batch size the pooled state last carried: the size of the
	// first handle slab beyond the inline ones, so a steady-state batch takes
	// one slab instead of growing through several.
	hint   int
	inline [pipelineInlineCmds]cmdRef
}

// pipelineBig is a Pipeline allocated together with room for pipelineBigCmds
// handles. The embedded Pipeline is what callers hold; the interior pointer
// keeps the whole allocation alive.
type pipelineBig struct {
	Pipeline
	slots [pipelineBigCmds]cmdRef
}

// pipeState is the recyclable part of a Pipeline: its buffers. A Pipeline
// owns one from Client.Pipeline until Release, which resets it and returns it
// to pipelinePool for the next Client.Pipeline call.
type pipeState struct {
	// w accumulates wire bytes across AppendCommand calls.
	w *protocol.Writer

	// cmds owns the per-command tracking state by value. addCmd hands out
	// indexes into it; typed cmd handles store the index and resolve it
	// inside Result(). Capacity is grown once (on first overflow) and
	// preserved across pool cycles.
	cmds []pipeCmd

	// reqs is reused across Exec calls to avoid allocating a fresh
	// []*redisRequest per call.
	reqs []*redisRequest

	// argsScratch backs variadic arg builds (HSet, LPush, ZAdd, ...). We
	// reuse it across addCmd calls within a single Exec; on Reset it is
	// truncated but its backing is retained.
	argsScratch []string

	// strSlab is a contiguous buffer holding all string results from the
	// direct-extraction pipeline path. unsafe.String references into this
	// slab are stored in pipeCmd.str. The slab is owned by the pipeState
	// (not the conn), so strings remain valid until the next Exec/Discard/
	// Release — matching the documented lifetime of typed Cmd handles.
	strSlab []byte

	// reqSlab is a contiguous buffer of redisRequest objects reused across
	// Exec calls. Instead of 1000 individual sync.Pool round-trips for a
	// 1000-command pipeline, we allocate one slab and point reqs[i] into it.
	// Each entry's doneCh is allocated lazily on first use and retained for
	// subsequent Exec calls. The slab only grows and is never shrunk.
	reqSlab []redisRequest

	// hint is len(cmds) of the batch that was last released with this state.
	hint int

	// executed is set once Exec has sent the queued commands and cleared by
	// Discard/Release. A second Exec returns errPipelineExecuted instead of
	// waiting for replies to commands it never sent.
	executed bool

	// failed is set when Exec failed after sending. The command list is kept
	// so each Result reports the real error, but the batch is over: the next
	// command queued starts a new one (the pipeline stays usable, as in
	// v1.5.11), and an Exec with nothing new queued sends nothing.
	failed bool
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

// pipelinePool recycles the buffers of released Pipelines. The common
// "per-request Pipeline" pattern then allocates one object per Pipeline after
// warmup: the Pipeline value, which carries room for its typed Cmd handles
// (about 450 bytes for up to 16 commands, about 4 KiB for 17 to 128).
var pipelinePool = sync.Pool{
	New: func() any {
		return &pipeState{
			w:           protocol.NewWriterSize(4096),
			cmds:        make([]pipeCmd, 0, defaultPipelineCap),
			reqs:        make([]*redisRequest, 0, defaultPipelineCap),
			argsScratch: make([]string, 0, 8),
			strSlab:     make([]byte, 0, 1024),
			reqSlab:     make([]redisRequest, 0, defaultPipelineCap),
		}
	},
}

// Pipeline returns a Pipeline bound to c. Call [Pipeline.Release] after
// reading results to return its buffers to the pool for reuse; every value
// read from the Pipeline is invalid after Release.
func (c *Client) Pipeline() *Pipeline {
	st := pipelinePool.Get().(*pipeState)
	var p *Pipeline
	if st.hint > pipelineInlineCmds && st.hint <= pipelineBigCmds {
		b := &pipelineBig{}
		p = &b.Pipeline
		p.refs = b.slots[:0]
	} else {
		p = &Pipeline{}
		p.refs = p.inline[:0]
	}
	p.client = c
	p.hint = st.hint
	p.st.Store(st)
	return p
}

// Release returns p's buffers to the pool. After Release, every *StringCmd,
// *IntCmd, ... previously returned from p is invalid: its Result returns an
// error, and the strings and results already read from it must not be used
// (the buffers belong to the next Pipeline). A released Pipeline rejects
// every call with an error and never reaches another caller's Pipeline.
// Release is idempotent. Calling Release is optional; an un-Released Pipeline
// is simply GC'd like any other heap object.
func (p *Pipeline) Release() {
	if p == nil {
		return
	}
	st := p.st.Swap(nil)
	if st == nil {
		return
	}
	if n := len(st.cmds); n > 0 {
		st.hint = n
	}
	st.reset()
	// Release oversized slabs to the GC so a single spike (e.g. 100K
	// GETs returning 1KB each) does not permanently grow the pooled
	// buffers' memory footprint.
	if cap(st.strSlab) > maxSlabRetain {
		st.strSlab = nil
	}
	if cap(st.reqSlab)*int(unsafe.Sizeof(redisRequest{})) > maxSlabRetain {
		st.reqSlab = nil
	}
	pipelinePool.Put(st)
}

// reset clears queued commands while preserving slab capacity so the
// Pipeline can be handed to a new caller without re-allocating its
// internals.
func (p *pipeState) reset() {
	p.resetRequests()
	// Zero used slots so previously-retained pointers (if any leaked) don't
	// observe stale values.
	for i := range p.cmds {
		p.cmds[i] = pipeCmd{}
	}
	p.cmds = p.cmds[:0]
	p.strSlab = p.strSlab[:0]
	p.executed = false
	p.failed = false
}

// newBatch starts a new batch after a failed Exec. It is reset() except that
// strSlab keeps its contents: strings already read from the failed batch stay
// valid until Discard or Release, as documented.
func (p *Pipeline) newBatch() {
	keep := p.strSlab
	p.reset()
	p.strSlab = keep
}

// resetRequests clears the wire buffer and the per-request state of the last
// Exec, leaving the command list and its results alone.
func (p *pipeState) resetRequests() {
	p.w.Reset()
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
}

// Discard drops all queued commands without sending anything. The Pipeline
// remains usable for subsequent enqueues. Discard on a released Pipeline is a
// no-op: it never touches the buffers, which belong to the next Pipeline.
func (p *Pipeline) Discard() {
	if st := p.st.Load(); st != nil {
		st.reset()
		p.refs = p.refs[:0]
	}
}

// Exec sends every queued command in one write and blocks until the last
// reply is received. Returns the first parse/transport error; per-command
// errors are reachable via each *Cmd.Result().
//
// A Pipeline runs once. After Exec returned an error, the next step depends on
// the failure:
//
//	failure                                    next step
//	no connection obtained                     Exec again: the queue is intact
//	context deadline or cancel, dropped conn   queue the commands again, Exec
//	a command got a server error reply         Discard first: Exec returns an
//	                                           error at once, sends nothing
//
// After a successful Exec, a second Exec returns an error at once (it does not
// block or touch a connection): Discard before queueing a new batch.
func (p *Pipeline) Exec(ctx context.Context) error {
	st := p.st.Load()
	if st == nil {
		return errPipelineReleased
	}
	return st.exec(ctx, p.client)
}

func (p *pipeState) exec(ctx context.Context, client *Client) error {
	if p.executed {
		return errPipelineExecuted
	}
	n := len(p.cmds)
	if n == 0 || p.failed {
		return nil
	}
	conn, err := client.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	p.executed = true
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
		// Keep the command list so each Result reports the real error (a
		// deadline, a closed conn) instead of "released"; a command whose
		// request never came back gets the error too. The batch is over: the
		// next queued command starts a new one.
		for i := range p.cmds {
			if p.cmds[i].err == nil && !p.cmds[i].direct && p.cmds[i].val == nil {
				p.cmds[i].err = err
			}
		}
		p.resetRequests()
		p.executed = false
		p.failed = true
		// Give the pool slot back (celeris#1268). Every error execManyCore
		// returns has closed the conn first (as does the ErrClosed of a conn
		// that was already closed), and releaseCmd discards a closed conn, so
		// Open drops and the slot is freed. Without this a failed Exec kept
		// its slot for good.
		client.pool.releaseCmd(conn)
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
	// A reply error that came with the conn closing (the server dropped it)
	// is a failed Exec like the one above, not a command's error reply: the
	// Pipeline stays usable. Read it before the conn goes back to the pool.
	connLost := firstErr != nil && conn.closed.Load()
	client.pool.releaseCmd(conn)
	p.w.Reset()
	// Leave p.cmds populated so the user can read Result() off each typed
	// handle. The slabs are fully reset only on Release/Discard.
	p.reqs = p.reqs[:0]
	if connLost {
		p.resetRequests()
		p.executed = false
		p.failed = true
	}
	return firstErr
}

// transferResult moves the dispatch result from a redisRequest into a pipeCmd.
// When direct extraction was used, the typed result is copied into pipeCmd's
// inline fields and string data is re-homed into the Pipeline's strSlab.
// Otherwise a heap-allocated *Value is stored so the user can read it via
// the as*() helpers.
func (p *pipeState) transferResult(pc *pipeCmd, r *redisRequest) {
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
		// sync pipeline case uses the direct path above. On the sync path an
		// untyped r.result may alias the conn's copySlab, which the next
		// Exec on the conn rewrites, so detach it before the caller keeps it
		// (celeris#1265). Already-detached values are copied once more; this
		// path is rare.
		v := copyValueDetached(r.result)
		pc.val = &v
	}
}

// slabStr appends the bytes of s into the Pipeline-owned strSlab and returns
// a string backed by the slab memory via unsafe.String. This avoids a
// per-string heap allocation — the entire pipeline's string results share one
// contiguous slab. The returned string is valid until reset/Release.
func (p *pipeState) slabStr(s string) string {
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
func (p *pipeState) addCmd(kind cmdKind, args ...string) int {
	if p.failed {
		p.newBatch()
	}
	p.w.AppendCommand(args...)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// addCmd2 is the 2-arg fast path that avoids the variadic []string alloc.
func (p *pipeState) addCmd2(kind cmdKind, a0, a1 string) int {
	if p.failed {
		p.newBatch()
	}
	p.w.AppendCommand2(a0, a1)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// addCmd3 is the 3-arg fast path.
func (p *pipeState) addCmd3(kind cmdKind, a0, a1, a2 string) int {
	if p.failed {
		p.newBatch()
	}
	p.w.AppendCommand3(a0, a1, a2)
	p.cmds = append(p.cmds, pipeCmd{kind: kind})
	return len(p.cmds) - 1
}

// ---------- Deferred result handles ----------
//
// Each typed Cmd stores a back-pointer to the owning Pipeline and the index
// into the Pipeline's command list. Result() resolves the command at call
// time: the Pipeline's current state, or an error once it was released.
// Handles live in Pipeline.refs, which is never recycled, so a handle kept
// past Release can only ever reach its own (released) Pipeline.
//
// For Tx (which allocates each pipeCmd individually), the pc field is set
// directly and p is nil. Result() checks pc first.

// cmdRef is the representation shared by every typed Cmd (StringCmd,
// IntCmd, ...): they have identical underlying types, so a *cmdRef from the
// handle slab converts to the typed pointer without a copy.
type cmdRef struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

// releasedRef is the handle returned for a command queued on a released
// Pipeline. It is shared and read-only: its Result reports the error.
var releasedRef = cmdRef{pc: errPipeCmdOrphaned}

func (c *cmdRef) cmd() *pipeCmd {
	if c.pc != nil {
		return c.pc
	}
	if c.p == nil {
		return errPipeCmdOrphaned
	}
	st := c.p.st.Load()
	if st == nil || c.idx >= len(st.cmds) {
		return errPipeCmdOrphaned
	}
	return &st.cmds[c.idx]
}

// StringCmd is a deferred string reply.
type StringCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *StringCmd) cmd() *pipeCmd { return (*cmdRef)(c).cmd() }

// Result returns the decoded string and error.
func (c *StringCmd) Result() (string, error) {
	pc := c.cmd()
	if pc.err != nil {
		return "", pc.err
	}
	if pc.direct {
		return pc.str, nil
	}
	if pc.val == nil {
		return "", errCmdNotExecuted
	}
	return asString(*pc.val)
}

// IntCmd is a deferred int64 reply.
type IntCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *IntCmd) cmd() *pipeCmd { return (*cmdRef)(c).cmd() }

// Result returns the int64 and error.
func (c *IntCmd) Result() (int64, error) {
	pc := c.cmd()
	if pc.err != nil {
		return 0, pc.err
	}
	if pc.direct {
		return pc.scalar, nil
	}
	if pc.val == nil {
		return 0, errCmdNotExecuted
	}
	return asInt(*pc.val)
}

// StatusCmd is a deferred simple-string reply.
type StatusCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *StatusCmd) cmd() *pipeCmd { return (*cmdRef)(c).cmd() }

// Result returns the status string and error.
func (c *StatusCmd) Result() (string, error) {
	pc := c.cmd()
	if pc.err != nil {
		return "", pc.err
	}
	if pc.direct {
		return pc.str, nil
	}
	if pc.val == nil {
		return "", errCmdNotExecuted
	}
	return asString(*pc.val)
}

// FloatCmd is a deferred float reply.
type FloatCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *FloatCmd) cmd() *pipeCmd { return (*cmdRef)(c).cmd() }

// Result returns the float64 and error.
func (c *FloatCmd) Result() (float64, error) {
	pc := c.cmd()
	if pc.err != nil {
		return 0, pc.err
	}
	if pc.direct {
		return math.Float64frombits(uint64(pc.scalar)), nil
	}
	if pc.val == nil {
		return 0, errCmdNotExecuted
	}
	return asFloat(*pc.val)
}

// BoolCmd is a deferred bool reply.
type BoolCmd struct {
	pc  *pipeCmd
	p   *Pipeline
	idx int
}

func (c *BoolCmd) cmd() *pipeCmd { return (*cmdRef)(c).cmd() }

// Result returns the bool and error.
func (c *BoolCmd) Result() (bool, error) {
	pc := c.cmd()
	if pc.err != nil {
		return false, pc.err
	}
	if pc.direct {
		return pc.scalar != 0, nil
	}
	if pc.val == nil {
		return false, errCmdNotExecuted
	}
	return asBool(*pc.val)
}

// ---------- Typed-cmd handles ----------

// newRef returns the handle for command idx. The handle lives in p.refs,
// which starts as the inline array and moves to a fresh slab when full. A
// full slab is left to the handles that point into it.
func (p *Pipeline) newRef(idx int) *cmdRef {
	if len(p.refs) == cap(p.refs) {
		n := pipelineOverflowCmds
		switch {
		case cap(p.refs) > pipelineInlineCmds:
			n = 2 * cap(p.refs)
		case p.hint > len(p.refs):
			n = p.hint - len(p.refs)
		}
		p.refs = make([]cmdRef, 0, n)
	}
	p.refs = append(p.refs, cmdRef{p: p, idx: idx})
	return &p.refs[len(p.refs)-1]
}

func (p *Pipeline) newIntCmd(idx int) *IntCmd       { return (*IntCmd)(p.newRef(idx)) }
func (p *Pipeline) newStringCmd(idx int) *StringCmd { return (*StringCmd)(p.newRef(idx)) }
func (p *Pipeline) newStatusCmd(idx int) *StatusCmd { return (*StatusCmd)(p.newRef(idx)) }
func (p *Pipeline) newBoolCmd(idx int) *BoolCmd     { return (*BoolCmd)(p.newRef(idx)) }

// ---------- Pipeline commands ----------
//
// Every builder loads the pooled state once; a nil state means the Pipeline
// was released, and the command is not queued: the returned handle's Result
// reports the error and Exec fails.

// Get enqueues GET.
func (p *Pipeline) Get(key string) *StringCmd {
	st := p.st.Load()
	if st == nil {
		return (*StringCmd)(&releasedRef)
	}
	return p.newStringCmd(st.addCmd2(kindString, "GET", key))
}

// Set enqueues SET with optional EX/PX.
func (p *Pipeline) Set(key string, value any, expiration time.Duration) *StatusCmd {
	st := p.st.Load()
	if st == nil {
		return (*StatusCmd)(&releasedRef)
	}
	if expiration <= 0 {
		return p.newStatusCmd(st.addCmd3(kindStatus, "SET", key, argify(value)))
	}
	st.argsScratch = append(st.argsScratch[:0], "SET", key, argify(value))
	st.argsScratch = appendExpire(st.argsScratch, expiration)
	return p.newStatusCmd(st.addCmd(kindStatus, st.argsScratch...))
}

// SetBytes is the allocation-lean variant of [Pipeline.Set] — uses
// unsafe.String to avoid the argify string(x) copy.
func (p *Pipeline) SetBytes(key string, value []byte, expiration time.Duration) *StatusCmd {
	st := p.st.Load()
	if st == nil {
		return (*StatusCmd)(&releasedRef)
	}
	if expiration <= 0 {
		return p.newStatusCmd(st.addCmd3(kindStatus, "SET", key, unsafeStringFromBytes(value)))
	}
	st.argsScratch = append(st.argsScratch[:0], "SET", key, unsafeStringFromBytes(value))
	st.argsScratch = appendExpire(st.argsScratch, expiration)
	return p.newStatusCmd(st.addCmd(kindStatus, st.argsScratch...))
}

// Del enqueues DEL.
func (p *Pipeline) Del(keys ...string) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	if len(keys) == 1 {
		return p.newIntCmd(st.addCmd2(kindInt, "DEL", keys[0]))
	}
	st.argsScratch = append(st.argsScratch[:0], "DEL")
	st.argsScratch = append(st.argsScratch, keys...)
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// Incr enqueues INCR.
func (p *Pipeline) Incr(key string) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	return p.newIntCmd(st.addCmd2(kindInt, "INCR", key))
}

// Decr enqueues DECR.
func (p *Pipeline) Decr(key string) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	return p.newIntCmd(st.addCmd2(kindInt, "DECR", key))
}

// Exists enqueues EXISTS.
func (p *Pipeline) Exists(keys ...string) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	if len(keys) == 1 {
		return p.newIntCmd(st.addCmd2(kindInt, "EXISTS", keys[0]))
	}
	st.argsScratch = append(st.argsScratch[:0], "EXISTS")
	st.argsScratch = append(st.argsScratch, keys...)
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// HGet enqueues HGET.
func (p *Pipeline) HGet(key, field string) *StringCmd {
	st := p.st.Load()
	if st == nil {
		return (*StringCmd)(&releasedRef)
	}
	return p.newStringCmd(st.addCmd3(kindString, "HGET", key, field))
}

// HSet enqueues HSET.
func (p *Pipeline) HSet(key string, values ...any) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	st.argsScratch = append(st.argsScratch[:0], "HSET", key)
	for _, v := range values {
		st.argsScratch = append(st.argsScratch, argify(v))
	}
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// LPush enqueues LPUSH.
func (p *Pipeline) LPush(key string, values ...any) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	st.argsScratch = append(st.argsScratch[:0], "LPUSH", key)
	for _, v := range values {
		st.argsScratch = append(st.argsScratch, argify(v))
	}
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// RPush enqueues RPUSH.
func (p *Pipeline) RPush(key string, values ...any) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	st.argsScratch = append(st.argsScratch[:0], "RPUSH", key)
	for _, v := range values {
		st.argsScratch = append(st.argsScratch, argify(v))
	}
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// SAdd enqueues SADD.
func (p *Pipeline) SAdd(key string, members ...any) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	st.argsScratch = append(st.argsScratch[:0], "SADD", key)
	for _, m := range members {
		st.argsScratch = append(st.argsScratch, argify(m))
	}
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// ZAdd enqueues ZADD.
func (p *Pipeline) ZAdd(key string, members ...Z) *IntCmd {
	st := p.st.Load()
	if st == nil {
		return (*IntCmd)(&releasedRef)
	}
	st.argsScratch = append(st.argsScratch[:0], "ZADD", key)
	for _, m := range members {
		st.argsScratch = append(st.argsScratch, strconv.FormatFloat(m.Score, 'f', -1, 64), argify(m.Member))
	}
	return p.newIntCmd(st.addCmd(kindInt, st.argsScratch...))
}

// Expire enqueues EXPIRE.
func (p *Pipeline) Expire(key string, d time.Duration) *BoolCmd {
	st := p.st.Load()
	if st == nil {
		return (*BoolCmd)(&releasedRef)
	}
	secs := int64(d / time.Second)
	if secs <= 0 {
		secs = 1
	}
	return p.newBoolCmd(st.addCmd3(kindBool, "EXPIRE", key, strconv.FormatInt(secs, 10)))
}

// addCmd queues a raw command on the live state and returns its index, or -1
// when the Pipeline was released. Used by the cluster pipeline's sub-pipelines.
func (p *Pipeline) addCmd(kind cmdKind, args ...string) int {
	st := p.st.Load()
	if st == nil {
		return -1
	}
	return st.addCmd(kind, args...)
}

// pipeCmds returns the live command list, or nil once released.
func (p *Pipeline) pipeCmds() []pipeCmd {
	if st := p.st.Load(); st != nil {
		return st.cmds
	}
	return nil
}
