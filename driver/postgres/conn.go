package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/postgres/protocol"
	"github.com/goceleris/celeris/engine"
)

// maxDirectResultBytes is the per-query result buffer cap in direct mode.
// Direct mode pins syncMode=true (lazy streaming cannot activate because
// the caller goroutine IS the dispatcher — a blocking channel send would
// deadlock), so without a cap a SELECT against a huge table would buffer
// every row into req.rowSlab. 64 MiB matches the WebSocket detach-limit
// on the engine side and gives typical result sets a wide margin.
const maxDirectResultBytes = 64 << 20

// syncRoundTripper is the optional interface for event-loop workers that
// support a combined write+read fast path. On Linux, the standalone eventloop
// worker implements this; on other platforms it's nil.
type syncRoundTripper interface {
	WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// syncBusyRoundTripper is the no-yield variant used by drivers opened
// WithEngine (caller is a LockOSThread'd celeris HTTP engine worker).
type syncBusyRoundTripper interface {
	WriteAndPollBusy(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (ok bool, err error)
}

// pgSyncLoop adapts the mini-loop's two sync variants behind a single
// WriteAndPoll method. Opened-WithEngine conns set busy=<impl> to route
// through WriteAndPollBusy (skips runtime.Gosched — locked-M-safe);
// standalone conns set busy=nil and route through the yielding variant.
type pgSyncLoop struct {
	sync syncRoundTripper
	busy syncBusyRoundTripper
}

func (s *pgSyncLoop) WriteAndPoll(fd int, data []byte, rbuf []byte, onRecv func([]byte)) (bool, error) {
	if s.busy != nil {
		return s.busy.WriteAndPollBusy(fd, data, rbuf, onRecv)
	}
	return s.sync.WriteAndPoll(fd, data, rbuf, onRecv)
}

// useBusySync enables the no-yield busy-poll variant when the driver was
// opened WithEngine. The handler that invokes us will run on the celeris
// HTTP engine's LockOSThread'd worker, where runtime.Gosched triggers
// stoplockedm+startlockedm futex storms.
func (c *pgConn) useBusySync(enable bool) {
	if !enable || c.syncLoop == nil {
		return
	}
	if sb, ok := c.loop.(syncBusyRoundTripper); ok {
		c.syncLoop.busy = sb
	}
}

// requestKind tags the in-flight pgRequest so the event loop's recv handler
// can pick the right protocol state machine.
type requestKind int

const (
	reqSimple requestKind = iota
	reqExtended
	reqStartup
	reqPrepare
	reqCopyIn
	reqCopyOut
)

// pgRequest represents a single round trip on a pgConn. Fields are filled on
// the event-loop goroutine and read on the goroutine that called
// QueryContext / ExecContext / Ping after doneCh closes.
//
// pgRequest is pooled (see pgRequestPool) so the per-op struct / channel /
// slice allocations amortize across queries. Callers must obtain a fresh
// instance via acquirePgRequest and release via releasePgRequest after
// consuming its result — the pool owns the underlying slab buffers that back
// rows/rowFields/rowSlab so they must not be retained by the caller.
type pgRequest struct {
	ctx      context.Context
	kind     requestKind
	simple   protocol.SimpleQueryState
	extended protocol.ExtendedQueryState
	startup  *protocol.StartupState
	prep     *prepareState
	copyIn   *protocol.CopyInState
	copyOut  *protocol.CopyOutState
	// copyReady fires (exactly once) when a CopyInResponse arrives; the
	// driver goroutine waits on it before streaming CopyData frames. We use
	// a dedicated channel so the goroutine that called CopyFrom can block
	// without consuming doneCh (which still signals terminal completion).
	copyReady     chan struct{}
	copyReadyOnce sync.Once
	// onCopyRow is invoked on the event-loop goroutine for each CopyData
	// payload that arrives during COPY OUT. The slice aliases the Reader's
	// buffer, so callers must copy if they want to retain bytes.
	onCopyRow func([]byte) error
	columns   []protocol.ColumnDesc
	// rows holds copies (not aliases) of every DataRow payload; the event
	// loop hands us slices into the Reader's internal buffer, so we must
	// copy before the next Feed.
	//
	// Storage layout to amortize allocations for small result sets:
	//   rowFields — a single backing [][]byte that we carve into one
	//               row-view per DataRow. Each row-view is row-slice
	//               into rowFields.
	//   rowSlab   — a single []byte slab we append to for every
	//               non-nil field. Each field-slice into rows[n][k]
	//               is rowSlab[start:end].
	// Both are owned by the pool; Reset truncates (cap retained).
	rows      [][][]byte
	rowFields [][]byte
	rowSlab   []byte
	tag       string
	err       error
	doneCh    chan struct{}
	// streaming fields: when rowCh is non-nil, dispatch sends each DataRow
	// to rowCh instead of buffering into rows/rowFields/rowSlab. colsCh is
	// signaled once RowDescription is parsed so the caller can return a
	// streamRows before all data arrives.
	rowCh     chan [][]byte
	colsCh    chan struct{}
	streamErr atomic.Pointer[error] // set before close(rowCh) so readers see the error race-free
	// syncMode suppresses the lazy streaming promotion (promoteToStreaming)
	// when the request is being handled on the sync fast path
	// (WriteAndPoll). The sync path runs dispatch on the calling goroutine,
	// so a blocking channel send would deadlock. When syncMode is true,
	// dispatch buffers all rows in req.rows regardless of count. The sync
	// fast path clears this flag before falling through to the async wait
	// so that subsequent dispatches can promote to streaming if needed.
	// Atomic because the caller goroutine clears it while the event-loop
	// goroutine may concurrently read it in dispatch.
	syncMode atomic.Bool
	fromPool bool
	// doneFlag / doneMu guard close(doneCh) so concurrent completion sites —
	// the event loop's dispatch (onRecv), failAll (onClose), and failReq
	// (Write error) — cannot double-close. We use a (mu, bool) pair rather
	// than sync.Once because the pgRequest is pooled and reused; sync.Once's
	// internal state cannot be reset in-place (it embeds a mutex whose
	// runtime lock chain would be corrupted by a zero-value overwrite).
	doneMu   sync.Mutex
	doneFlag bool
	// doneAtom is the lockless mirror of doneFlag. The spin-wait fast path
	// checks it without acquiring doneMu. Set to 1 by finish()/fail()
	// immediately before signaling doneCh; reset to 0 in acquirePgRequest.
	doneAtom atomic.Uint32
	// finished is set atomically as the very last action in finish()/fail(),
	// AFTER the mutex is released and the channel is signaled. It guards
	// pool-put: releasePgRequest only returns the request to the pool when
	// finished is true, preventing a race where a new acquirePgRequest
	// reuses the request while finish()/fail() is still reading its fields.
	finished atomic.Bool
}

// finish signals completion of the request exactly once. Safe for
// concurrent callers.
//
// Semantics are slightly unusual because the pgRequest is pooled: for
// pooled instances we send on a buffered (cap=1) channel and the wait()
// path does a single recv, so the channel is reusable across life-cycles
// without close. For non-pooled instances (legacy code paths and tests)
// the channel is unbuffered and we fall back to close-broadcast — wait()
// works the same whether it receives a value or sees a closed channel.
func (r *pgRequest) finish() {
	r.doneMu.Lock()
	if r.doneFlag {
		r.doneMu.Unlock()
		return
	}
	r.doneFlag = true
	r.doneAtom.Store(1)
	ch := r.doneCh
	fromPool := r.fromPool
	r.doneMu.Unlock()
	if fromPool {
		// Non-blocking send — buffer size 1, flag-guarded against re-entry.
		select {
		case ch <- struct{}{}:
		default:
		}
		r.finished.Store(true)
		return
	}
	close(ch)
	r.finished.Store(true)
}

// fail records err on r and signals completion exactly once. Only the first
// caller's err wins — subsequent failers (e.g. failReq racing with onClose's
// failAll on the same request) are no-ops. Safe for concurrent callers.
//
// Callers that already hold exclusive access (e.g. the event-loop dispatch
// goroutine, which serializes all dispatch and all onClose callbacks) may
// continue to set r.err directly and call r.finish(); fail() is for the
// two-goroutine paths where failReq (caller) races failAll (event loop).
func (r *pgRequest) fail(err error) {
	r.doneMu.Lock()
	if r.doneFlag {
		r.doneMu.Unlock()
		return
	}
	r.doneFlag = true
	r.err = err
	r.doneAtom.Store(1)
	ch := r.doneCh
	fromPool := r.fromPool
	r.doneMu.Unlock()
	if fromPool {
		select {
		case ch <- struct{}{}:
		default:
		}
		r.finished.Store(true)
		return
	}
	close(ch)
	r.finished.Store(true)
}

// pgRequestPool amortizes pgRequest + its embedded state machines + its
// doneCh + rows/rowFields/rowSlab across queries on the hot path. We do NOT
// pool the copy machinery (copyReady/copyIn/copyOut/onCopyRow): those rare
// paths rebuild their own pgRequest directly.
var pgRequestPool = sync.Pool{
	New: func() any {
		return &pgRequest{
			// Pre-sized for the common "single column, single row" result
			// so SELECT 1 / SELECT id FROM t WHERE ... never re-grows.
			rows:      make([][][]byte, 0, 4),
			rowFields: make([][]byte, 0, 8),
			rowSlab:   make([]byte, 0, 128),
			// Buffered size-1 channel so finish() performs a non-blocking
			// send and wait() a recv. Reusable across life-cycles (drained
			// on acquire), unlike close(chan) which is one-shot.
			doneCh: make(chan struct{}, 1),
		}
	},
}

// acquirePgRequest returns a reset pgRequest ready for reqSimple or
// reqExtended use. Callers must set ctx/kind and either .simple/.extended
// state via the embedded value fields before enqueuing.
func acquirePgRequest() *pgRequest {
	r := pgRequestPool.Get().(*pgRequest)
	// Guard against the pool returning a request whose previous finish()
	// or fail() call has not yet fully returned (the event-loop goroutine
	// may still be reading fields after the handler goroutine called
	// releasePgRequest). If finished is not set, discard and allocate new.
	if !r.finished.Load() {
		r = pgRequestPool.New().(*pgRequest)
	}
	r.finished.Store(false)
	r.fromPool = true
	// Drain any stale signal left on the buffered doneCh (e.g. if the
	// previous life-cycle completed but wait()'s ctx fired first and it
	// returned before consuming the signal). The channel stays reusable:
	// no allocation on the hot path.
	select {
	case <-r.doneCh:
	default:
	}
	// Reset the completion flags so finish() can signal the (now-drained)
	// channel. doneMu is NOT reset — its zero value is always a valid
	// unlocked mutex, and a prior life cycle always left it unlocked.
	r.doneFlag = false
	r.doneAtom.Store(0)
	return r
}

// releasePgRequest returns r to the pool. The caller must have already
// consumed any row/columns data before calling — the slabs are truncated
// in place and subsequent Gets will overwrite them.
func releasePgRequest(r *pgRequest) {
	if r == nil || !r.fromPool {
		return
	}
	// Only return to the pool if finish()/fail() has fully completed.
	// If the event-loop goroutine is still inside finish(), drop the
	// request and let GC reclaim it — acquirePgRequest will allocate
	// a fresh one instead.
	if !r.finished.Load() {
		return
	}
	r.ctx = nil
	r.kind = 0
	// Preserve the scratch slices inside each state machine so subsequent
	// life cycles reuse the backing arrays.
	r.simple.Reset()
	r.extended.Reset()
	r.startup = nil
	r.prep = nil
	r.copyIn = nil
	r.copyOut = nil
	r.copyReady = nil
	r.copyReadyOnce = sync.Once{}
	r.onCopyRow = nil
	r.columns = nil
	// Clear refs in the carved rows so we don't pin old alias targets.
	for i := range r.rows {
		r.rows[i] = nil
	}
	r.rows = r.rows[:0]
	for i := range r.rowFields {
		r.rowFields[i] = nil
	}
	r.rowFields = r.rowFields[:0]
	r.rowSlab = r.rowSlab[:0]
	r.tag = ""
	r.err = nil
	r.rowCh = nil
	r.syncMode.Store(false)
	r.colsCh = nil
	r.streamErr.Store(nil)
	pgRequestPool.Put(r)
}

// appendRowFromAlias copies fields (which alias the Reader's recv buffer)
// into the pgRequest's row slab and appends a row-view into r.rows. It
// preserves the nil-vs-empty distinction for PG NULLs (nil) vs zero-length
// text (non-nil empty slice).
//
// Aliasing correctness: the slab grows with append, which may reallocate
// the backing array. Any sub-slice taken before the reallocation would be
// stale. We avoid the problem by growing the slab to its final size first
// (single pre-append if capacity is insufficient) and then handing out
// sub-slices against the now-stable backing array.
func (r *pgRequest) appendRowFromAlias(fields [][]byte) {
	totalBytes := 0
	for _, f := range fields {
		totalBytes += len(f)
	}
	// Ensure the slab has stable storage for every non-nil field. We do
	// this in one shot so later sub-slices (view[i]) can point into the
	// same backing array and stay valid.
	if totalBytes > 0 {
		if cap(r.rowSlab)-len(r.rowSlab) < totalBytes {
			// Grow with append+len to get amortized 2x behavior.
			need := len(r.rowSlab) + totalBytes
			// Use make+copy so we can size the new slab to fit without
			// walking through a sequence of incremental appends.
			ns := make([]byte, len(r.rowSlab), growCap(cap(r.rowSlab), need))
			copy(ns, r.rowSlab)
			r.rowSlab = ns
		}
	}
	// Ensure rowFields has stable storage for the row-view. One grow if
	// needed, then a no-realloc append for the row itself.
	if cap(r.rowFields)-len(r.rowFields) < len(fields) {
		need := len(r.rowFields) + len(fields)
		nf := make([][]byte, len(r.rowFields), growCap(cap(r.rowFields), need))
		copy(nf, r.rowFields)
		r.rowFields = nf
	}
	start := len(r.rowFields)
	r.rowFields = r.rowFields[:start+len(fields)]
	view := r.rowFields[start : start+len(fields)]
	for i, f := range fields {
		if f == nil {
			view[i] = nil
			continue
		}
		if len(f) == 0 {
			view[i] = r.rowSlab[len(r.rowSlab):len(r.rowSlab)]
			continue
		}
		slabStart := len(r.rowSlab)
		r.rowSlab = append(r.rowSlab, f...)
		view[i] = r.rowSlab[slabStart : slabStart+len(f)]
	}
	r.rows = append(r.rows, view)
}

// growCap returns a capacity for a slice growth from old to at least need.
// Mirrors runtime.growslice heuristics (double up to ~1KB, then 1.25x).
func growCap(old, need int) int {
	if old < 32 {
		old = 32
	}
	for old < need {
		if old < 1024 {
			old *= 2
		} else {
			old = old + old/4
		}
	}
	return old
}

// Ctx implements async.PendingRequest.
func (r *pgRequest) Ctx() context.Context { return r.ctx }

// OnRowDesc implements [protocol.SimpleQueryObserver]. Called once per
// RowDescription frame on simple-query requests. Stashes the column
// list and unblocks any caller waiting on colsCh for early streamRows
// return.
func (r *pgRequest) OnRowDesc(cols []protocol.ColumnDesc) {
	r.columns = cols
	if r.rowCh != nil && r.colsCh != nil {
		close(r.colsCh)
	}
}

// OnRow implements [protocol.SimpleQueryObserver] and
// [protocol.ExtendedQueryObserver]. Called once per DataRow.
//
// Direct-mode (sync syncMode) buffers everything into rowSlab/rows;
// streaming mode forwards through rowCh. The simple and extended
// dispatch branches share this method — the only difference between
// them is whether onRowDesc fires (extended takes columns from the
// Describe step, not from a per-DataRow signal).
func (r *pgRequest) OnRow(fields [][]byte) {
	if r.rowCh != nil {
		r.rowCh <- copyRow(fields)
		return
	}
	r.appendRowFromAlias(fields)
	// Direct-mode result-buffer cap. In direct mode syncMode is pinned
	// so lazy streaming never fires; without a cap a huge SELECT would
	// buffer every row in memory unbounded. Cap at maxDirectResultBytes
	// (64 MiB) and fail with ErrResultTooBig — caller paginates or
	// switches modes.
	if r.syncMode.Load() && len(r.rowSlab) > maxDirectResultBytes {
		r.doneMu.Lock()
		if r.err == nil {
			r.err = ErrResultTooBig
		}
		r.doneMu.Unlock()
		return
	}
	// Skip streaming promotion when colsCh is nil (Exec paths don't
	// allocate one; promoting would panic on close(nil colsCh)).
	if !r.syncMode.Load() && r.colsCh != nil && len(r.rows) >= streamThreshold {
		promoteToStreaming(r)
	}
}

// prepareState drives a Parse + Describe S + Sync exchange. Unlike
// ExtendedQueryState, it does not expect a Bind or Execute.
type prepareState struct {
	name   string
	query  string
	params []uint32
	cols   []protocol.ColumnDesc
	err    *protocol.PGError
	// phase:
	//   0: expecting ParseComplete
	//   1: expecting ParameterDescription
	//   2: expecting RowDescription / NoData
	//   3: expecting ReadyForQuery
	phase int
}

// handlePrepare processes a single server message for a prepare exchange.
// Returns done=true on ReadyForQuery.
func (p *prepareState) handlePrepare(msgType byte, payload []byte) (bool, error) {
	switch msgType {
	case protocol.BackendErrorResponse:
		p.err = protocol.ParseErrorResponse(payload)
		return false, nil
	case protocol.BackendReadyForQuery:
		if p.err != nil {
			return true, p.err
		}
		return true, nil
	case protocol.BackendParseComplete:
		p.phase = 1
		return false, nil
	case protocol.BackendParameterDesc:
		oids, err := protocol.ParseParameterDescription(payload)
		if err != nil {
			return false, err
		}
		p.params = oids
		p.phase = 2
		return false, nil
	case protocol.BackendRowDescription:
		cols, err := protocol.ParseRowDescription(payload)
		if err != nil {
			return false, err
		}
		p.cols = cols
		p.phase = 3
		return false, nil
	case protocol.BackendNoData:
		p.cols = nil
		p.phase = 3
		return false, nil
	case protocol.BackendNoticeResponse, protocol.BackendParameterStatus, protocol.BackendNotification:
		return false, nil
	}
	// Ignore anything unexpected (we may see out-of-band notifications).
	return false, nil
}

// pgConn is a single PostgreSQL connection driven by an event loop.
type pgConn struct {
	fd int
	// fdFile keeps the *os.File returned by net.TCPConn.File() reachable for
	// the lifetime of the pgConn. Without this reference, GC can finalize
	// the file and close our dup'd fd out from under the event loop (observed
	// as sporadic EBADF / hangs under GC pressure).
	fdFile    *os.File
	loop      engine.WorkerLoop
	syncLoop  *pgSyncLoop // nil on non-Linux or no sync support
	workerID  int
	closeLoop func()
	syncBuf   []byte // read buffer for syncLoop.WriteAndPoll

	// Direct mode: used when the caller runs on an unlocked G (standalone
	// or Config.AsyncHandlers=true). All I/O goes through c.tcp (which is
	// backed by Go's netpoll) instead of the mini-loop — no LockOSThread'd
	// worker in the picture, so net.Conn.Read parks the G cleanly without
	// the futex storm that Gosched would cause on a locked M.
	//
	// In direct mode, loop is nil, syncLoop is nil, and every write goes
	// through writeRaw → tcp.Write; every wait drives tcp.Read → onRecv
	// on the caller goroutine.
	useDirect bool
	tcp       *net.TCPConn
	tcpFd     int        // cached fd for MSG_DONTWAIT peek; 0 if unavailable
	directMu  sync.Mutex // serializes tcp.Write
	directBuf []byte     // read buffer for driveDirect

	addr   *net.TCPAddr
	opts   Options
	dsn    DSN
	pid    int32
	secret int32

	reader *protocol.Reader

	// writerMu guards writer. All message-building sites (simpleQuery /
	// simpleExec / extendedQuery / extendedExec / prepareStatement /
	// closePreparedServer / Close's Terminate / startup Handle) reuse the
	// same Writer buffer for allocation-free encoding. Because Close can
	// run concurrently with any of those from the caller goroutine, the
	// buffer must be serialized — otherwise one goroutine's Reset can land
	// between another's StartMessage/FinishMessage and corrupt the bytes.
	writerMu sync.Mutex
	writer   *protocol.Writer

	bridge *async.Bridge

	// pending mirrors bridge — we need a typed reference to the current
	// request on recv to avoid repeated type assertions on the hot path.
	pendingMu sync.Mutex
	pending   []*pgRequest

	stmtCache *lru
	// autoCache, when true, routes cacheable SELECT-style QueryContext
	// calls through the per-conn stmtCache even when the caller did not
	// explicitly Prepare. Opt-in via DSN's AutoCacheStatements option.
	autoCache bool

	createdAt  time.Time
	lastUsedAt atomic.Int64 // unix nanos

	maxLifetime time.Duration
	maxIdleTime time.Duration

	closeOnce sync.Once
	closeErr  atomic.Value // error
	closed    atomic.Bool
	// closeWG tracks background goroutines spawned by the conn
	// (dropPreparedAsync, etc.) so Close can join them and we don't
	// leak Gs that outlive the conn.
	closeWG sync.WaitGroup

	// fdCloseOnce guards syscall.Close(fd) so it runs exactly once regardless
	// of whether the first close path is Close() or onClose() (fired by the
	// event loop on peer EOF / error). The event loop's UnregisterConn does
	// NOT close the fd — see driver/{epoll,iouring} "caller is responsible
	// for closing the underlying fd" — so the driver retains fd ownership.
	fdCloseOnce sync.Once

	stmtCounter uint64

	// sessionDirty tracks whether the connection state has been modified in a
	// way that requires DISCARD ALL on return to the pool. Only set by
	// BeginTx (transactions change GUC/temp table state). PrepareContext and
	// extended queries do NOT set this flag — prepared statements are kept
	// alive across pool returns and re-prepared on miss (SQLSTATE 26000).
	sessionDirty atomic.Bool

	serverParamsMu sync.RWMutex
	serverParams   map[string]string
}

// closeFDOnce closes c.fd exactly once. Safe to call from Close() and from
// onClose() concurrently; only the first winner actually issues the close.
// We close via c.fdFile.Close() so the *os.File finalizer is satisfied and
// cannot later run against an fd that may have been reused by the kernel.
func (c *pgConn) closeFDOnce() {
	c.fdCloseOnce.Do(func() {
		if c.fdFile != nil {
			_ = c.fdFile.Close()
			return
		}
		if c.fd > 0 {
			_ = syscall.Close(c.fd)
		}
	})
}

// buildMessage serializes writer access. fn runs under writerMu and may use
// c.writer freely; the returned bytes are a freshly-owned copy that fn can
// safely hand off to the event loop after the lock is released.
func (c *pgConn) buildMessage(fn func(*protocol.Writer) []byte) []byte {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	return fn(c.writer)
}

// writeRaw sends data to the server using whichever transport the conn was
// opened with. Direct-mode conns bypass the event loop entirely: they dial
// a *net.TCPConn whose Read/Write go through Go's netpoll. Loop-mode conns
// hand the bytes to the mini-loop (or the engine's loop for WithEngine,
// non-async mode).
func (c *pgConn) writeRaw(data []byte) error {
	if c.useDirect {
		c.directMu.Lock()
		_, err := c.tcp.Write(data)
		c.directMu.Unlock()
		return err
	}
	return c.loop.Write(c.fd, data)
}

// driveDirect is the direct-mode equivalent of mini-loop's WriteAndPoll
// read phase: it spins in a tight tcp.Read → c.onRecv loop on the caller
// goroutine until the request completes (req.doneAtom != 0) or the ctx
// is cancelled. The caller must have already issued writeRaw for the
// request's payload before invoking driveDirect.
//
// Concurrency: in direct mode the pool pins a conn to exactly one
// caller goroutine at a time, so we're the sole reader on c.tcp. That
// also means c.reader / c.onRecv state mutations are single-threaded
// (no event-loop goroutine reads here), matching loop mode's invariant
// that onRecv always runs on exactly one goroutine per conn.
func (c *pgConn) driveDirect(ctx context.Context, req *pgRequest) error {
	if c.directBuf == nil {
		c.directBuf = make([]byte, 16<<10)
	}
	for {
		if req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			return req.err
		}
		if err := ctx.Err(); err != nil {
			if c.pid != 0 && c.secret != 0 {
				_ = sendCancelRequest(ctx, c.addr, c.pid, c.secret)
			}
			// Bounded wait for the server's Error + RFQ response so we
			// leave the wire aligned. If the server doesn't drain within
			// 30s we fail the conn to prevent a stuck reader.
			deadline := time.Now().Add(30 * time.Second)
			_ = c.tcp.SetReadDeadline(deadline)
			for req.doneAtom.Load() == 0 {
				n, rerr := c.tcp.Read(c.directBuf)
				if n > 0 {
					c.onRecv(c.directBuf[:n])
				}
				if rerr != nil {
					break
				}
			}
			_ = c.tcp.SetReadDeadline(time.Time{})
			return err
		}
		if c.tcpFd > 0 {
			if n, _, perr := syscall.Recvfrom(c.tcpFd, c.directBuf, syscall.MSG_DONTWAIT); n > 0 {
				c.onRecv(c.directBuf[:n])
				continue
			} else if perr != nil && perr != syscall.EAGAIN && perr != syscall.EWOULDBLOCK {
				if errors.Is(perr, io.EOF) {
					perr = io.ErrUnexpectedEOF
				}
				c.failAll(perr)
				_ = c.closeDirect()
				return perr
			}
		}
		n, err := c.tcp.Read(c.directBuf)
		if n > 0 {
			c.onRecv(c.directBuf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			c.failAll(err)
			_ = c.closeDirect()
			return err
		}
	}
}

// closeDirect tears down the direct-mode tcp connection exactly once.
func (c *pgConn) closeDirect() error {
	if c.tcp == nil {
		return nil
	}
	return c.tcp.Close()
}

// startDirectReader spawns a goroutine that pumps tcp.Read → onRecv for
// operations that need async server message delivery (COPY FROM/TO,
// potentially LISTEN). Direct-mode conns have no event-loop goroutine
// driving onRecv, so these flows would otherwise block on copyReady /
// doneCh forever. The returned stop func halts the reader and restores
// the connection's read deadline; callers must defer it.
//
// The reader uses a short read deadline (50ms) to periodically check
// the stop signal — this avoids needing a separate wakeup path while
// keeping worst-case shutdown latency bounded.
func (c *pgConn) startDirectReader() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 16<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.tcp.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, err := c.tcp.Read(buf)
			if n > 0 {
				c.onRecv(buf[:n])
			}
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				c.failAll(err)
				return
			}
		}
	}()
	return func() {
		close(stop)
		// Unstick any pending tcp.Read by setting the deadline to now,
		// then wait for the goroutine to observe stop and exit.
		_ = c.tcp.SetReadDeadline(time.Now())
		<-done
		_ = c.tcp.SetReadDeadline(time.Time{})
	}
}

// dialConn dials a TCP connection to the DSN's host:port, sets it non-blocking,
// and registers it with the given event loop. The returned pgConn is fully
// initialized (including startup) and ready for queries.
// dialConn accepts closeLoop for symmetry with Pool.dialConn; callers that
// own the loop themselves (e.g. [Connector.Connect]) pass nil.
//
//nolint:unparam // closeLoop is always nil today but retained for pool reuse
func dialConn(ctx context.Context, prov engine.EventLoopProvider, closeLoop func(), dsn DSN, workerHint int) (*pgConn, error) {
	if err := dsn.CheckSSL(); err != nil {
		return nil, err
	}
	addr, err := resolveAddr(dsn.Host, dsn.Port)
	if err != nil {
		return nil, err
	}

	dialTimeout := dsn.Options.ConnectTimeout
	dialer := net.Dialer{Timeout: dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("celeris-postgres: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	file, err := tcp.File()
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	// tcp.File dup'd the fd; close the net.TCPConn wrapper. We own the dup'd
	// fd from here on. CRITICAL: *os.File carries a runtime finalizer that
	// closes the fd on GC — if file becomes unreachable the finalizer shuts
	// the fd out from under the event loop (observed as sporadic EBADF /
	// hangs under GC pressure in high-rate benchmarks). Store file in the
	// pgConn so it stays reachable for the conn's lifetime; closeFDOnce()
	// closes via file.Close() so the finalizer is satisfied.
	fd := int(file.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = tcp.Close()
		_ = file.Close()
		return nil, err
	}
	tuneConnSocket(fd)
	_ = tcp.Close()

	nw := prov.NumWorkers()
	if nw <= 0 {
		// Use file.Close() (not syscall.Close(fd)) so the *os.File's
		// runtime finalizer is disarmed. Otherwise a later GC cycle would
		// call syscall.Close(fd) a second time, potentially on an
		// unrelated fd the kernel has since reassigned to another socket.
		_ = file.Close()
		return nil, errors.New("celeris-postgres: event loop has 0 workers")
	}
	if workerHint < 0 || workerHint >= nw {
		workerHint = fd % nw
		if workerHint < 0 {
			workerHint = -workerHint
		}
	}
	loop := prov.WorkerLoop(workerHint)

	// Cache the SyncRoundTripper capability for the fast path.
	var syncL *pgSyncLoop
	if srt, ok := loop.(syncRoundTripper); ok {
		syncL = &pgSyncLoop{sync: srt}
	}

	c := &pgConn{
		fd:        fd,
		fdFile:    file,
		loop:      loop,
		syncLoop:  syncL,
		workerID:  workerHint,
		closeLoop: closeLoop,
		addr:      addr,
		opts:      dsn.Options,
		dsn:       dsn,
		reader:    protocol.NewReader(),
		writer:    protocol.NewWriter(),
		bridge:    async.NewBridge(),
		stmtCache: newLRU(dsn.Options.StatementCacheSize),
		autoCache: dsn.Options.AutoCacheStatements && dsn.Options.StatementCacheSize > 0,
		// Pre-size pending queue for the common case (pipeline depth 1-2).
		// Backing cap is retained across enqueue/popHead cycles thanks to
		// the in-place shift in popHead, so bursty depths only allocate
		// on the first surge.
		pending:      make([]*pgRequest, 0, 8),
		createdAt:    time.Now(),
		serverParams: map[string]string{},
	}
	if syncL != nil {
		c.syncBuf = make([]byte, 16<<10) // 16 KiB read buffer for sync path
	}
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := loop.RegisterConn(fd, c.onRecv, c.onClose); err != nil {
		// Close via file.Close() to disarm the *os.File finalizer. A stray
		// syscall.Close(fd) here would leave the finalizer armed; when GC
		// later fires it would close the same fd a SECOND time, possibly
		// hitting an unrelated fd the kernel reassigned in the meantime.
		_ = file.Close()
		return nil, err
	}

	if err := c.doStartup(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func resolveAddr(host, port string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port))
}

// dialDirectConn opens a pgConn that bypasses the event loop entirely.
// All I/O goes through c.tcp (Go netpoll) on the caller goroutine; there
// is no worker M holding the conn, no LockOSThread involved, and no
// syncLoop. Used when the engine is absent (standalone) or when the
// engine dispatches HTTP handlers asynchronously (handler runs on an
// unlocked G, so net.Conn.Read parks cleanly without a futex storm).
func dialDirectConn(ctx context.Context, dsn DSN) (*pgConn, error) {
	if err := dsn.CheckSSL(); err != nil {
		return nil, err
	}
	addr, err := resolveAddr(dsn.Host, dsn.Port)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: dsn.Options.ConnectTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("celeris-postgres: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	// Cache the raw fd for MSG_DONTWAIT peek. Best-effort: if
	// SyscallConn fails we just fall through to plain tcp.Read.
	var rawFd int
	if sc, scErr := tcp.SyscallConn(); scErr == nil {
		_ = sc.Control(func(fd uintptr) { rawFd = int(fd) })
	}

	c := &pgConn{
		useDirect:    true,
		tcp:          tcp,
		tcpFd:        rawFd,
		addr:         addr,
		opts:         dsn.Options,
		dsn:          dsn,
		reader:       protocol.NewReader(),
		writer:       protocol.NewWriter(),
		bridge:       async.NewBridge(),
		stmtCache:    newLRU(dsn.Options.StatementCacheSize),
		autoCache:    dsn.Options.AutoCacheStatements && dsn.Options.StatementCacheSize > 0,
		pending:      make([]*pgRequest, 0, 8),
		createdAt:    time.Now(),
		serverParams: map[string]string{},
	}
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := c.doStartup(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// doStartup performs the full startup handshake synchronously. The event loop
// delivers bytes to onRecv, which drives the StartupState machine via a
// dedicated pgRequest of kind reqStartup.
func (c *pgConn) doStartup(ctx context.Context) error {
	st := &protocol.StartupState{
		User:     c.dsn.User,
		Password: c.dsn.Password,
		Database: c.dsn.Database,
		Params:   c.dsn.Params,
	}
	initial := c.buildMessage(func(w *protocol.Writer) []byte { return st.Start(w) })
	req := &pgRequest{
		ctx:     ctx,
		kind:    reqStartup,
		startup: st,
		doneCh:  make(chan struct{}),
	}
	c.enqueue(req)
	if err := c.writeRaw(initial); err != nil {
		c.failReq(req, err)
		return err
	}
	if err := c.wait(ctx, req); err != nil {
		return err
	}
	c.pid = st.PID
	c.secret = st.Secret
	c.serverParamsMu.Lock()
	c.serverParams = st.ServerParams
	c.serverParamsMu.Unlock()
	return nil
}

// enqueue appends req to both the typed pending slice and the bridge.
//
// Both pending queues amortize allocations by retaining capacity across
// Pop'd entries — steady-state depth is O(1) for serialized users
// (database/sql acquires a conn, runs a query, returns the conn), so the
// append path only allocates when a bursty caller drives queue depth past
// the current cap.
func (c *pgConn) enqueue(req *pgRequest) {
	c.pendingMu.Lock()
	c.pending = append(c.pending, req)
	c.pendingMu.Unlock()
	c.bridge.Enqueue(req)
}

// popHead removes the front-of-queue request.
//
// To keep the pending slice's backing capacity stable across the
// enqueue+pop lifecycle typical of serialized query use, we shift
// remaining entries down instead of re-slicing with [1:] (which sheds
// capacity one element at a time and forces the next Enqueue to realloc).
//
//nolint:unparam // callers sometimes ignore the return; kept symmetric
func (c *pgConn) popHead() *pgRequest {
	c.pendingMu.Lock()
	n := len(c.pending)
	if n == 0 {
		c.pendingMu.Unlock()
		return nil
	}
	r := c.pending[0]
	if n == 1 {
		c.pending[0] = nil
		c.pending = c.pending[:0]
	} else {
		copy(c.pending, c.pending[1:])
		c.pending[n-1] = nil
		c.pending = c.pending[:n-1]
	}
	c.pendingMu.Unlock()
	_ = c.bridge.Pop()
	return r
}

// failReq drops req from the queue and surfaces err to its waiting goroutine.
//
// IMPORTANT: failReq MUST only be called when no response bytes for req can
// possibly have reached the server — i.e. immediately after an enqueue+Write
// where Write returned an error (the bytes never left the buffer). Calling
// failReq after the server may have begun replying desynchronizes the
// PG wire: the reply would pop whatever request now sits at the head of the
// queue. If req is not the tail of pending, we panic — that state is
// unreachable under the intended contract and indicates a programming bug.
func (c *pgConn) failReq(req *pgRequest, err error) {
	c.pendingMu.Lock()
	idx := -1
	for i, r := range c.pending {
		if r == req {
			idx = i
			break
		}
	}
	if idx < 0 {
		c.pendingMu.Unlock()
		// Already removed (e.g. concurrent onClose drained the queue). Fall
		// through: fail() is idempotent and first-err-wins, so we cannot
		// stomp the err already set by failAll racing on another goroutine.
		req.fail(err)
		return
	}
	if idx != len(c.pending)-1 {
		c.pendingMu.Unlock()
		panic("celeris-postgres: failReq called for a non-tail request; PG responses are FIFO and would desync")
	}
	c.pending = c.pending[:idx]
	c.pendingMu.Unlock()
	// Keep the bridge in lock-step with pending; otherwise the bridge retains
	// a phantom tail entry that will steal a future reply.
	_ = c.bridge.PopTail()
	req.fail(err)
}

// wait blocks until req.doneCh closes or ctx is canceled. On cancel, we fire
// a side-channel CancelRequest and continue waiting for the server's response
// (which will be an Error + RFQ) — this avoids racing the event loop.
//
// Performance: we spin-check req.doneAtom (an atomic flag set by finish/fail
// immediately before signaling doneCh) for a brief window before parking on
// the channel. The spin avoids the futex park/wake round trip when the PG
// response arrives within the window (the common case for local queries).
// The atomic check is ~1ns per iteration; 128 iterations is ~0.1µs, which
// costs nothing but catches the already-done case.
func (c *pgConn) wait(ctx context.Context, req *pgRequest) error {
	if c.useDirect {
		return c.driveDirect(ctx, req)
	}
	// Phase 1: tight spin — atomic check only, ~1ns per iteration. Catches
	// the already-done case (response arrived during the sync poll or write).
	for range 64 {
		if req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			return req.err
		}
	}
	// Phase 2: yield-assisted spin — Gosched lets the event-loop worker
	// goroutine run and deliver the response. Catches localhost PG replies
	// that arrive within ~50-100µs without the ~2-5µs futex park/wake.
	for range 64 {
		runtime.Gosched()
		if req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			return req.err
		}
	}
	// Fall through to blocking select.
	select {
	case <-req.doneCh:
		return req.err
	case <-ctx.Done():
		if c.pid != 0 && c.secret != 0 {
			_ = sendCancelRequest(ctx, c.addr, c.pid, c.secret)
		}
		// Wait for the server to drain the query response (Error + RFQ),
		// but bound the wait so a network partition cannot block forever.
		cancelTimer := time.NewTimer(30 * time.Second)
		select {
		case <-req.doneCh:
			cancelTimer.Stop()
			if req.err != nil {
				return req.err
			}
			return ctx.Err()
		case <-cancelTimer.C:
			c.failAll(errors.New("celeris-postgres: cancel timeout"))
			return ctx.Err()
		}
	}
}

// onRecv is the event-loop callback. data aliases the worker's receive buffer
// and is valid only for the duration of this call.
func (c *pgConn) onRecv(data []byte) {
	c.reader.Feed(data)
	for {
		msgType, payload, err := c.reader.Next()
		if errors.Is(err, protocol.ErrIncomplete) {
			break
		}
		if err != nil {
			c.failAll(err)
			return
		}
		if err := c.dispatch(msgType, payload); err != nil {
			c.failAll(err)
			return
		}
	}
	c.reader.Compact()
}

// dispatch feeds one message into the state machine of the current head
// request.
func (c *pgConn) dispatch(msgType byte, payload []byte) error {
	// Connection-level async messages are consumed without touching the
	// head request.
	switch msgType {
	case protocol.BackendParameterStatus:
		pos := 0
		k, err := protocol.ReadCString(payload, &pos)
		if err == nil {
			v, _ := protocol.ReadCString(payload, &pos)
			c.serverParamsMu.Lock()
			if c.serverParams == nil {
				c.serverParams = map[string]string{}
			}
			c.serverParams[k] = v
			c.serverParamsMu.Unlock()
		}
		return nil
	case protocol.BackendNoticeResponse, protocol.BackendNotification:
		return nil
	}

	c.pendingMu.Lock()
	if len(c.pending) == 0 {
		c.pendingMu.Unlock()
		return fmt.Errorf("celeris-postgres: unexpected server message %q with empty queue", msgType)
	}
	head := c.pending[0]
	c.pendingMu.Unlock()

	switch head.kind {
	case reqStartup:
		var resp []byte
		var done bool
		var err error
		c.writerMu.Lock()
		resp, done, err = head.startup.Handle(msgType, payload, c.writer)
		c.writerMu.Unlock()
		// Accumulate error under doneMu so a concurrent failAll (from
		// onClose on another goroutine) cannot race on head.err.
		if err != nil {
			head.doneMu.Lock()
			head.err = err
			head.doneMu.Unlock()
		}
		if resp != nil {
			if werr := c.writeRaw(resp); werr != nil {
				head.doneMu.Lock()
				if head.err == nil {
					head.err = werr
				}
				head.doneMu.Unlock()
			}
		}
		if done || err != nil {
			c.popHead()
			head.finish()
		}
	case reqSimple:
		// pgRequest implements protocol.SimpleQueryObserver via OnRowDesc /
		// OnRow methods — passing head directly avoids the per-dispatch
		// closure-pair allocation the previous inline funcs paid (-2
		// allocs/op on the hot path; see PR description for full delta).
		done, err := head.simple.Handle(msgType, payload, head)
		if err != nil {
			if head.rowCh != nil {
				head.streamErr.Store(&err)
				close(head.rowCh)
				if head.colsCh != nil {
					select {
					case <-head.colsCh:
					default:
						close(head.colsCh)
					}
				}
			}
			c.popHead()
			head.fail(err)
			return nil
		}
		if done {
			if head.rowCh != nil {
				close(head.rowCh)
			}
			c.popHead()
			head.finish()
		}
	case reqExtended:
		// pgRequest implements protocol.ExtendedQueryObserver via the
		// shared OnRow method — same allocation-free dispatch as the
		// simple-query branch.
		done, err := head.extended.Handle(msgType, payload, head)
		if err != nil {
			if head.rowCh != nil {
				head.streamErr.Store(&err)
				close(head.rowCh)
				if head.colsCh != nil {
					select {
					case <-head.colsCh:
					default:
						close(head.colsCh)
					}
				}
			}
			c.popHead()
			head.fail(err)
			return nil
		}
		if done {
			head.tag = head.extended.Tag
			if head.columns == nil {
				head.columns = head.extended.Columns
			}
			if head.rowCh != nil {
				if head.colsCh != nil {
					select {
					case <-head.colsCh:
					default:
						close(head.colsCh)
					}
				}
				close(head.rowCh)
			}
			c.popHead()
			head.finish()
		} else if head.rowCh != nil && head.colsCh != nil {
			// For extended queries, RowDescription comes from the Describe
			// response (before DataRow). Signal colsCh as soon as columns
			// are available so the caller can return streamRows early.
			if head.extended.Columns != nil && head.columns == nil {
				head.columns = head.extended.Columns
				select {
				case <-head.colsCh:
				default:
					close(head.colsCh)
				}
			}
		}
	case reqPrepare:
		done, err := head.prep.handlePrepare(msgType, payload)
		if err != nil {
			head.doneMu.Lock()
			head.err = err
			head.doneMu.Unlock()
		}
		if done {
			c.popHead()
			head.finish()
		}
	case reqCopyIn:
		done, err := head.copyIn.Handle(msgType, payload)
		if err != nil {
			c.popHead()
			head.fail(err)
			return nil
		}
		// Signal the driver goroutine as soon as the server is ready to
		// accept CopyData.
		if head.copyIn.Ready() {
			head.copyReadyOnce.Do(func() { close(head.copyReady) })
		}
		if done {
			head.doneMu.Lock()
			if head.copyIn.Err != nil && head.err == nil {
				head.err = head.copyIn.Err
			}
			head.doneMu.Unlock()
			if head.tag == "" {
				head.tag = head.copyIn.Tag
			}
			c.popHead()
			// Ensure a blocked driver goroutine waiting on copyReady is
			// unblocked if the server short-circuits without CopyInResponse
			// (e.g. ErrorResponse before CopyInResponse).
			head.copyReadyOnce.Do(func() { close(head.copyReady) })
			head.finish()
		}
	case reqCopyOut:
		done, err := head.copyOut.Handle(msgType, payload, func(row []byte) {
			if head.onCopyRow == nil {
				return
			}
			// Copy out of the Reader's buffer before handing to the user
			// callback — the slice aliases the worker's recv buffer.
			cp := make([]byte, len(row))
			copy(cp, row)
			if cbErr := head.onCopyRow(cp); cbErr != nil {
				head.doneMu.Lock()
				if head.err == nil {
					head.err = cbErr
				}
				head.doneMu.Unlock()
			}
		})
		if err != nil {
			c.popHead()
			head.fail(err)
			return nil
		}
		if done {
			head.doneMu.Lock()
			if head.copyOut.Err != nil && head.err == nil {
				head.err = head.copyOut.Err
			}
			head.doneMu.Unlock()
			if head.tag == "" {
				head.tag = head.copyOut.Tag
			}
			c.popHead()
			head.finish()
		}
	}
	return nil
}

func copyRow(fields [][]byte) [][]byte {
	row := make([][]byte, len(fields))
	for i, f := range fields {
		if f == nil {
			continue
		}
		cp := make([]byte, len(f))
		copy(cp, f)
		row[i] = cp
	}
	return row
}

// promoteToStreaming transitions a pgRequest from buffered mode to streaming
// mode. Called from dispatch (event-loop goroutine, single-threaded) when the
// accumulated row count hits streamThreshold. It allocates rowCh (the
// expensive ~2KB channel), drains the buffered rows into it, and signals
// colsCh so the waiting caller can begin consuming rows via streamRows.
//
// After this call, subsequent DataRows are sent directly to rowCh via
// copyRow. The buffered rows in req.rows are already slab-backed; we send
// them as-is into the channel (the slab memory transfers ownership).
func promoteToStreaming(req *pgRequest) {
	// Ensure columns are set before signaling the caller. For extended
	// queries, columns live on req.extended.Columns until the done branch
	// copies them to req.columns. We promote them now so the streamRows
	// returned by waitForQueryRows has valid column metadata.
	if req.columns == nil && req.kind == reqExtended {
		req.columns = req.extended.Columns
	}
	// Channel capacity must fit every buffered row without blocking — we hold
	// the dispatch goroutine here, and the caller is still selecting on colsCh
	// (which we close after this flush). Normally len(req.rows) == streamThreshold,
	// but the sync fast path (WriteAndPoll) suppresses promotion via
	// syncMode=true and may leave a large buffer behind when it falls through
	// to the async path. Sizing to len(req.rows) guarantees forward progress.
	chanCap := streamRowsChanSize
	if n := len(req.rows); n > chanCap {
		chanCap = n
	}
	req.rowCh = make(chan [][]byte, chanCap)
	for _, row := range req.rows {
		req.rowCh <- row
	}
	// Clear the buffer. We nil out the slice elements but retain the backing
	// capacity for reuse by releasePgRequest.
	for i := range req.rows {
		req.rows[i] = nil
	}
	req.rows = req.rows[:0]
	// Signal colsCh so the caller (waitForQueryRows) can return a streamRows.
	// colsCh was pre-allocated by simpleQuery/doExtendedQuery; Exec paths
	// don't allocate one and the dispatch sites above guard on colsCh != nil
	// before calling promoteToStreaming. Double-check here as a defense in
	// depth — close(nil) panics.
	if req.colsCh != nil {
		close(req.colsCh)
	}
}

// onClose is the event-loop callback for an fd-level shutdown. It closes the
// underlying fd (event loop does not own fd lifetime) via closeFDOnce, then
// fans the error out to any pending requests. Running concurrently with
// Close() is safe because both paths funnel through closeFDOnce and failAll
// is idempotent under closeOnce's covering atomic Store.
func (c *pgConn) onClose(err error) {
	if err == nil {
		err = io.EOF
	}
	c.closeErr.Store(err)
	c.closeFDOnce()
	c.failAll(err)
}

// failAll surfaces err to every pending request and marks the conn dead.
func (c *pgConn) failAll(err error) {
	c.closed.Store(true)
	c.pendingMu.Lock()
	queue := c.pending
	c.pending = nil
	c.pendingMu.Unlock()
	c.bridge.DrainWithError(err, nil)
	for _, r := range queue {
		if r == nil {
			continue
		}
		// Close streaming channels so the consumer unblocks.
		if r.rowCh != nil {
			r.streamErr.Store(&err)
			// Close rowCh exactly once: use a recover guard since another
			// goroutine (dispatch) might have already closed it.
			func() {
				defer func() { _ = recover() }()
				close(r.rowCh)
			}()
		}
		if r.colsCh != nil {
			func() {
				defer func() { _ = recover() }()
				close(r.colsCh)
			}()
		}
		// Use fail() so concurrent failReq (caller goroutine Write-error
		// path) and this failAll (event-loop onClose path) cannot race on
		// r.err. First winner's err is the one wait() observes.
		r.fail(err)
	}
}

// -------------------- driver.Conn methods -------------------------------

// Prepare implements driver.Conn using PrepareContext.
func (c *pgConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext parses and describes a statement, caching the result.
// Note: PrepareContext does NOT set sessionDirty — prepared statements are
// tracked by the LRU cache and cleaned up via DEALLOCATE ALL (not DISCARD
// ALL) on pool return, avoiding the expensive full-session reset.
func (c *pgConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	if cached, ok := c.stmtCache.get(query); ok {
		return &pgStmt{conn: c, prep: cached, query: query, cached: true}, nil
	}
	name := c.mintStmtName()
	prep, err := c.prepareStatement(ctx, name, query)
	if err != nil {
		return nil, err
	}
	evicted := c.stmtCache.put(query, prep)
	for _, ev := range evicted {
		// Fire-and-forget. We intentionally do NOT tie the eviction Close to
		// the caller's ctx: if the caller's ctx is about to be cancelled,
		// the Close 'S' + Sync must still be flushed or the server-side
		// named statement leaks for the lifetime of the connection. If the
		// server response never arrives, the request sits on this conn's
		// pending queue until the conn is torn down — at which point
		// failAll releases it. This is bounded by the conn lifetime.
		_ = c.closePreparedServerAsync(ev.Name)
	}
	return &pgStmt{conn: c, prep: prep, query: query, cached: true}, nil
}

func (c *pgConn) mintStmtName() string {
	n := atomic.AddUint64(&c.stmtCounter, 1)
	return fmt.Sprintf("celst_%d", n)
}

// prepareStatement runs Parse + Describe 'S' + Sync and returns the post-
// describe metadata.
func (c *pgConn) prepareStatement(ctx context.Context, name, query string) (*protocol.PreparedStmt, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	ps := &prepareState{name: name, query: query}
	req := &pgRequest{ctx: ctx, kind: reqPrepare, prep: ps, doneCh: make(chan struct{})}
	c.enqueue(req)

	payload := c.buildMessage(func(w *protocol.Writer) []byte {
		parseB := protocol.WriteParse(w, name, query, nil)
		descB := protocol.WriteDescribe(w, 'S', name)
		syncB := protocol.WriteSync(w)
		return joinBytes(parseB, descB, syncB)
	})
	if err := c.writeRaw(payload); err != nil {
		c.failReq(req, err)
		return nil, err
	}
	if err := c.wait(ctx, req); err != nil {
		return nil, err
	}
	return &protocol.PreparedStmt{
		Name:      ps.name,
		Query:     ps.query,
		ParamOIDs: ps.params,
		Columns:   ps.cols,
	}, nil
}

// closePreparedServerAsync is the fire-and-forget variant used for cache
// eviction. It enqueues the Close 'S' + Sync but does NOT block on the
// doneCh — if the server's reply never arrives, the pending request is
// released when the conn is torn down. This avoids tying a cache eviction
// to the evicting caller's ctx (PG-8/PG-12) while still bounding worst-case
// leakage to the conn's lifetime.
func (c *pgConn) closePreparedServerAsync(name string) error {
	if c.closed.Load() || name == "" {
		return nil
	}
	ps := &prepareState{name: name, phase: 3}
	// Use context.Background so the request is never cancelled by a caller.
	req := &pgRequest{ctx: context.Background(), kind: reqPrepare, prep: ps, doneCh: make(chan struct{})}
	c.enqueue(req)
	payload := c.buildMessage(func(w *protocol.Writer) []byte {
		closeB := protocol.WriteClose(w, 'S', name)
		syncB := protocol.WriteSync(w)
		return joinBytes(closeB, syncB)
	})
	if err := c.writeRaw(payload); err != nil {
		c.failReq(req, err)
		return err
	}
	return nil
}

// ExecContext satisfies driver.ExecerContext.
func (c *pgConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) == 0 {
		tag, _, err := c.simpleExec(ctx, query)
		if err != nil {
			return nil, err
		}
		return newPGResult(tag), nil
	}
	// Unnamed statement (stmtName="") is replaced on every Parse — no stale
	// state to clean up, so we don't mark the session dirty.
	return c.extendedExec(ctx, "", query, args)
}

// QueryContext satisfies driver.QueryerContext.
//
// When the DSN option AutoCacheStatements is enabled (opt-in; the LRU
// size is bounded by StatementCacheSize), cacheable SELECT-style queries
// are transparently auto-prepared on first use and reused via
// Bind+Describe+Execute+Sync on subsequent invocations. This matches
// pgx's QueryExecModeCacheStatement at steady state. The first call
// pays Parse + Describe + Bind + Execute in one flight (same as pgx);
// subsequent calls skip Parse.
func (c *pgConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.autoCache && isCacheableQuery(query) {
		if stmt, ok := c.stmtCache.get(query); ok {
			// Cached: skip the server-side Describe round-trip; we
			// already know the row description from the initial prep.
			return c.doExtendedQuery(ctx, stmt.Name, query, args, stmt.Columns)
		}
		// Cache miss with autoCache on — prepare + cache + execute.
		name := c.mintStmtName()
		prep, err := c.prepareStatement(ctx, name, query)
		if err == nil {
			evicted := c.stmtCache.put(query, prep)
			for _, ev := range evicted {
				c.dropPreparedAsync(ev)
			}
			return c.doExtendedQuery(ctx, name, query, args, prep.Columns)
		}
		// Prepare failed — fall through to the legacy path.
	}
	if len(args) == 0 {
		return c.simpleQuery(ctx, query)
	}
	return c.extendedQuery(ctx, "", query, args)
}

// isListenOrUnlisten reports whether q starts with LISTEN or UNLISTEN
// after skipping leading whitespace + comments. Direct-mode conns
// cannot support LISTEN because NotificationResponse messages arrive
// asynchronously between queries, and direct mode has no background
// reader between queries to consume them. Gating these statements
// with ErrDirectModeUnsupported prevents silently-dropped
// notifications (see #241 / #4 follow-up).
func isListenOrUnlisten(q string) bool {
	start := skipLeadingWSAndComments(q)
	rest := q[start:]
	for _, kw := range [...]string{"LISTEN", "UNLISTEN", "NOTIFY"} {
		if hasKeywordPrefix(rest, kw) {
			return true
		}
	}
	return false
}

// hasKeywordPrefix reports whether s starts with keyword followed
// by a non-identifier byte (whitespace, punctuation, end-of-string).
// This prevents false positives like "LISTENABLE" matching "LISTEN"
// or "SHOW_ME_THE_TABLES" matching "SHOW".
func hasKeywordPrefix(s, keyword string) bool {
	if !hasPrefixFold(s, keyword) {
		return false
	}
	if len(s) == len(keyword) {
		return true
	}
	c := s[len(keyword)]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';' || c == '(' || c == '-'
}

// skipLeadingWSAndComments returns the byte offset of the first
// non-whitespace, non-comment character in q.
func skipLeadingWSAndComments(q string) int {
	i := 0
	for i < len(q) {
		c := q[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		if c == '-' && i+1 < len(q) && q[i+1] == '-' {
			for i < len(q) && q[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(q) && q[i+1] == '*' {
			i += 2
			for i+1 < len(q) && (q[i] != '*' || q[i+1] != '/') {
				i++
			}
			i += 2
			continue
		}
		break
	}
	return i
}

// isCacheableQuery returns true when the query is a read that can safely
// be prepared and cached per-conn. The check is intentionally permissive
// — single-statement SELECT / WITH / VALUES / SHOW / TABLE. Anything
// else (DDL, SET, LISTEN, multi-statement, etc.) is routed through the
// simple protocol to preserve full-fidelity behaviour.
func isCacheableQuery(q string) bool {
	// Skip leading whitespace and SQL comments.
	for i := 0; i < len(q); {
		c := q[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		if c == '-' && i+1 < len(q) && q[i+1] == '-' {
			// Line comment — skip to newline.
			for i < len(q) && q[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(q) && q[i+1] == '*' {
			// Block comment — skip to */.
			i += 2
			for i+1 < len(q) && (q[i] != '*' || q[i+1] != '/') {
				i++
			}
			i += 2
			continue
		}
		// First non-whitespace, non-comment character: check keyword
		// with a word-boundary guard so we don't match statements
		// like "SHOW_TABLES" (procedure call) or "WITHDRAWAL" (a
		// table name).
		rest := q[i:]
		return hasKeywordPrefix(rest, "SELECT") ||
			hasKeywordPrefix(rest, "WITH") ||
			hasKeywordPrefix(rest, "VALUES") ||
			hasKeywordPrefix(rest, "SHOW") ||
			hasKeywordPrefix(rest, "TABLE")
	}
	return false
}

// hasPrefixFold reports whether s starts with prefix, ASCII case-insensitive.
// prefix must be uppercase.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		ch := s[i]
		if ch >= 'a' && ch <= 'z' {
			ch -= 'a' - 'A'
		}
		if ch != prefix[i] {
			return false
		}
	}
	return true
}

// dropPreparedAsync fires a best-effort Close('S') for an evicted
// prepared statement. Fire-and-forget to not block the caller, but
// bound by:
//   - a 5s timeout (so a stuck TCP write doesn't hold the G forever)
//   - the conn's closed flag (on Close, the G observes and bails)
//   - the conn's closeWG (Close waits so the G doesn't outlive
//     the conn's logical lifetime)
func (c *pgConn) dropPreparedAsync(prep *protocol.PreparedStmt) {
	if prep == nil || prep.Name == "" {
		return
	}
	if c.closed.Load() {
		return
	}
	c.closeWG.Add(1)
	go func() {
		defer c.closeWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if c.closed.Load() {
			return
		}
		_ = c.simpleExecNoTag(ctx, "DEALLOCATE "+prep.Name)
	}()
}

// simpleQuery runs a no-arg Query via the simple protocol.
//
// Lazy streaming: rows are accumulated in a slab-backed buffer (req.rows)
// by default. If the result exceeds streamThreshold rows, dispatch lazily
// allocates a bounded channel and switches to streaming mode. Small queries
// (the vast majority) never allocate a channel — zero regression vs the
// pre-streaming baseline.
//
// Sync fast path: if WriteAndPoll completes the entire response in one
// shot, we return a buffered pgRows directly from req.rows.
func (c *pgConn) simpleQuery(ctx context.Context, query string) (driver.Rows, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	if c.useDirect && isListenOrUnlisten(query) {
		return nil, ErrDirectModeUnsupported
	}
	req := acquirePgRequest()
	req.ctx = ctx
	req.kind = reqSimple
	// Lazy streaming: colsCh fires when promoteToStreaming activates
	// rowCh mid-dispatch, letting the caller goroutine switch to
	// streaming mode instead of waiting for the full result set.
	//
	// The alloc (~56 bytes) is unneeded in two hot paths:
	//   - useDirect: driveDirect owns the caller's read loop, no select
	//     on colsCh ever happens.
	//   - syncLoop fast path succeeds: simpleQuery returns directly
	//     from the buffered-rows branch without entering
	//     waitForQueryRows' select.
	// Skipping the allocation in useDirect shaves one alloc per
	// Query_1col_1row; the syncLoop fast path also benefits via the
	// fallback branch in waitForQueryRows which re-allocates when it
	// actually needs to wait.
	if !c.useDirect {
		req.colsCh = make(chan struct{})
	}
	// Direct mode drives onRecv on the caller goroutine; streaming
	// would deadlock the same way the sync fast path does, so pin
	// syncMode=true so dispatch never promotes.
	if c.useDirect {
		req.syncMode.Store(true)
	}
	c.enqueue(req)

	// Sync fast path: write the query and poll for the response on the
	// calling goroutine. If the entire response fits in one poll, return
	// a buffered pgRows directly from req.rows — zero channel overhead.
	// syncMode suppresses streaming promotion (which would deadlock: the
	// calling goroutine is the dispatch goroutine, so a blocking channel
	// send would never unblock).
	if c.syncLoop != nil {
		req.syncMode.Store(true)
		c.writerMu.Lock()
		payload := protocol.WriteQueryInto(c.writer, query)
		ok, err := c.syncLoop.WriteAndPoll(c.fd, payload, c.syncBuf, c.onRecv)
		c.writerMu.Unlock()
		if err != nil {
			c.failReq(req, err)
			releasePgRequest(req)
			return nil, err
		}
		if ok && req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			if req.err != nil {
				// Deferred mid-stream error with buffered rows.
				if len(req.rows) > 0 {
					c.touch()
					return acquirePGRows(req.columns, req.rows, true, req, req.err), nil
				}
				reqErr := req.err
				releasePgRequest(req)
				return nil, reqErr
			}
			c.touch()
			return acquirePGRows(req.columns, req.rows, true, req, nil), nil
		}
		// Partial or EAGAIN: clear syncMode so async dispatch can promote
		// to streaming if the result grows past the threshold.
		req.syncMode.Store(false)
	} else {
		// Async path: standard event-loop write.
		c.writerMu.Lock()
		payload := protocol.WriteQueryInto(c.writer, query)
		err := c.writeRaw(payload)
		c.writerMu.Unlock()
		if err != nil {
			c.failReq(req, err)
			releasePgRequest(req)
			return nil, err
		}
	}

	// Wait for completion or streaming activation.
	return c.waitForQueryRows(ctx, req, true)
}

// waitForQueryRows blocks until the request completes (buffered mode) or
// streaming is activated (lazy streaming). Shared by simpleQuery and
// doExtendedQuery.
//
// Two outcomes:
//   - Buffered (doneCh fires, rowCh == nil): returns a pgRows backed by req.rows.
//   - Streaming (colsCh fires first): returns a streamRows reading from rowCh.
//     colsCh fires when promoteToStreaming allocates rowCh on the Nth DataRow.
func (c *pgConn) waitForQueryRows(ctx context.Context, req *pgRequest, textFormat bool) (driver.Rows, error) {
	if c.useDirect {
		// Direct mode: no event-loop goroutine drives onRecv, so we drive
		// the read loop here until the request completes. Streaming mode
		// never activates (req.syncMode prevents promoteToStreaming) —
		// rows accumulate in req.rows and we return a buffered pgRows.
		if err := c.driveDirect(ctx, req); err != nil {
			if len(req.rows) > 0 {
				c.touch()
				return acquirePGRows(req.columns, req.rows, textFormat, req, err), nil
			}
			releasePgRequest(req)
			return nil, err
		}
		c.touch()
		return acquirePGRows(req.columns, req.rows, textFormat, req, nil), nil
	}
	select {
	case <-req.colsCh:
		// Streaming activated — promoteToStreaming allocated rowCh and
		// closed colsCh. Rows will continue streaming through rowCh.
		sr := &streamRows{
			columns:    req.columns,
			textFormat: textFormat,
			rowCh:      req.rowCh,
			doneCh:     req.doneCh,
			req:        req,
			errVal:     &req.streamErr,
		}
		sr.codecs = make([]*protocol.TypeCodec, len(req.columns))
		for i, col := range req.columns {
			sr.codecs[i] = protocol.LookupOID(col.TypeOID)
		}
		return sr, nil
	case <-req.doneCh:
		// Request fully completed. Drain the buffered doneCh signal.
		select {
		case <-req.doneCh:
		default:
		}
		if req.err != nil {
			// If rows were buffered before the error arrived (e.g.
			// mid-stream ErrorResponse), return them with the error
			// deferred to the end of iteration. If no rows, fail fast.
			if len(req.rows) > 0 {
				c.touch()
				return acquirePGRows(req.columns, req.rows, textFormat, req, req.err), nil
			}
			reqErr := req.err
			releasePgRequest(req)
			return nil, reqErr
		}
		c.touch()
		if req.rowCh != nil {
			// Streaming was activated and the request completed before we
			// entered this select. The rowCh is closed; drain it into pgRows.
			return drainStreamToPGRows(req, textFormat), nil
		}
		// Buffered fast path — no channel was ever allocated.
		return acquirePGRows(req.columns, req.rows, textFormat, req, nil), nil
	case <-ctx.Done():
		if c.pid != 0 && c.secret != 0 {
			_ = sendCancelRequest(ctx, c.addr, c.pid, c.secret)
		}
		<-req.doneCh
		reqErr := req.err
		releasePgRequest(req)
		if reqErr != nil {
			return nil, reqErr
		}
		return nil, ctx.Err()
	}
}

// drainStreamToPGRows drains all rows from req.rowCh (which must be closed)
// into a buffered pgRows. Used by the sync fast path when the entire response
// completed during WriteAndPoll — avoids the per-Next channel recv overhead
// for small result sets.
func drainStreamToPGRows(req *pgRequest, textFormat bool) *pgRows {
	var rows [][][]byte
	for row := range req.rowCh {
		rows = append(rows, row)
	}
	return acquirePGRows(req.columns, rows, textFormat, req, nil)
}

// simpleExec returns the command tag and rows-affected count. Callers may
// ignore the int64, which remains here for future rows-affected consumers.
//
//nolint:unparam // rows-affected return retained for future callers
func (c *pgConn) simpleExec(ctx context.Context, query string) (string, int64, error) {
	if c.closed.Load() {
		return "", 0, ErrClosed
	}
	if c.useDirect && isListenOrUnlisten(query) {
		return "", 0, ErrDirectModeUnsupported
	}
	req := acquirePgRequest()
	req.ctx = ctx
	req.kind = reqSimple
	c.enqueue(req)

	// Sync fast path: write + poll on the calling goroutine.
	if c.syncLoop != nil {
		c.writerMu.Lock()
		payload := protocol.WriteQueryInto(c.writer, query)
		ok, err := c.syncLoop.WriteAndPoll(c.fd, payload, c.syncBuf, c.onRecv)
		c.writerMu.Unlock()
		if err != nil {
			c.failReq(req, err)
			releasePgRequest(req)
			return "", 0, err
		}
		if ok && req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			if req.err != nil {
				reqErr := req.err
				releasePgRequest(req)
				return "", 0, reqErr
			}
			c.touch()
			tagBytes := req.simple.TagBytes()
			n, _ := protocol.RowsAffectedBytes(tagBytes)
			tag := string(tagBytes)
			releasePgRequest(req)
			return tag, n, nil
		}
		// EAGAIN or partial: fall through to blocking wait.
		if err := c.wait(ctx, req); err != nil {
			releasePgRequest(req)
			return "", 0, err
		}
		c.touch()
		tagBytes := req.simple.TagBytes()
		n, _ := protocol.RowsAffectedBytes(tagBytes)
		tag := string(tagBytes)
		releasePgRequest(req)
		return tag, n, nil
	}

	// Async path.
	c.writerMu.Lock()
	payload := protocol.WriteQueryInto(c.writer, query)
	err := c.writeRaw(payload)
	c.writerMu.Unlock()
	if err != nil {
		c.failReq(req, err)
		releasePgRequest(req)
		return "", 0, err
	}
	if err := c.wait(ctx, req); err != nil {
		releasePgRequest(req)
		return "", 0, err
	}
	c.touch()
	tagBytes := req.simple.TagBytes()
	n, _ := protocol.RowsAffectedBytes(tagBytes)
	tag := string(tagBytes)
	releasePgRequest(req)
	return tag, n, nil
}

// simpleExecNoTag is a lightweight variant of simpleExec for commands like
// BEGIN/COMMIT/ROLLBACK where the caller only needs the error. It skips
// tag extraction and rows-affected parsing, saving one string allocation.
func (c *pgConn) simpleExecNoTag(ctx context.Context, query string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if c.useDirect && isListenOrUnlisten(query) {
		return ErrDirectModeUnsupported
	}
	req := acquirePgRequest()
	req.ctx = ctx
	req.kind = reqSimple
	c.enqueue(req)

	if c.syncLoop != nil {
		c.writerMu.Lock()
		payload := protocol.WriteQueryInto(c.writer, query)
		ok, err := c.syncLoop.WriteAndPoll(c.fd, payload, c.syncBuf, c.onRecv)
		c.writerMu.Unlock()
		if err != nil {
			c.failReq(req, err)
			releasePgRequest(req)
			return err
		}
		if ok && req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			err = req.err
			releasePgRequest(req)
			if err != nil {
				return err
			}
			c.touch()
			return nil
		}
		if err = c.wait(ctx, req); err != nil {
			releasePgRequest(req)
			return err
		}
		releasePgRequest(req)
		c.touch()
		return nil
	}

	c.writerMu.Lock()
	payload := protocol.WriteQueryInto(c.writer, query)
	err := c.writeRaw(payload)
	c.writerMu.Unlock()
	if err != nil {
		c.failReq(req, err)
		releasePgRequest(req)
		return err
	}
	if err = c.wait(ctx, req); err != nil {
		releasePgRequest(req)
		return err
	}
	releasePgRequest(req)
	c.touch()
	return nil
}

// extendedQuery issues Parse(stmtName) + Bind + Describe P + Execute + Sync.
// When stmtName is empty, Parse uses the unnamed statement; when non-empty,
// a Parse for an already-server-known name is still valid (PG accepts
// re-Parse of the same query with the same OIDs).
func (c *pgConn) extendedQuery(ctx context.Context, stmtName, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.doExtendedQuery(ctx, stmtName, query, args, nil)
	if stmtName != "" && isPreparedStatementNotFound(err) {
		c.stmtCache.remove(query)
		prep, prepErr := c.prepareStatement(ctx, stmtName, query)
		if prepErr != nil {
			return nil, prepErr
		}
		c.stmtCache.put(query, prep)
		rows, err = c.doExtendedQuery(ctx, stmtName, query, args, prep.Columns)
	}
	return rows, err
}

func (c *pgConn) doExtendedQuery(ctx context.Context, stmtName, query string, args []driver.NamedValue, cachedCols []protocol.ColumnDesc) (driver.Rows, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	values, formats, encSlot, err := encodeArgs(args)
	if err != nil {
		return nil, err
	}
	defer releaseEncodedArgs(encSlot)
	req := acquirePgRequest()
	req.ctx = ctx
	req.kind = reqExtended
	// When the caller has cached column descriptions (auto-prepare or
	// explicit cache), we pre-populate them and drop the server-side
	// Describe round trip. Saves one message per query (~7 bytes of
	// wire and one protocol-state transition) — measured +3-5% RPS
	// on the MSR1 PG cell.
	//
	// Intentional check: cachedCols == nil (not len==0). A prepared
	// statement that genuinely returns zero columns (e.g. VALUES ()
	// or a no-projection query) has a non-nil but zero-length slice;
	// treating it as "no cache" would defeat the optimization and
	// send a redundant Describe for every call.
	hasDescribe := cachedCols == nil
	req.extended.HasDescribe = hasDescribe
	if !hasDescribe {
		// Prepare-time Describe returns FormatCode=0 (text) because
		// Postgres doesn't decide the output encoding until Execute,
		// which carries the resultFormats vector. We pass
		// [FormatBinary] below, so reflect that on the cached column
		// slice to keep decode on the binary path. Use a shallow copy
		// so the cached slice in stmtCache isn't mutated.
		cols := make([]protocol.ColumnDesc, len(cachedCols))
		copy(cols, cachedCols)
		for i := range cols {
			cols[i].FormatCode = protocol.FormatBinary
		}
		req.columns = cols
		req.extended.Columns = cols
	}
	// Lazy streaming: colsCh only needed in the async/streaming path.
	// useDirect never enters waitForQueryRows' select so the alloc is
	// unused there. See simpleQuery for the detailed rationale.
	if !c.useDirect {
		req.colsCh = make(chan struct{})
	}
	// Direct mode drives reads on the caller goroutine; streaming would
	// deadlock. Pin syncMode=true so dispatch never promotes.
	if c.useDirect {
		req.syncMode.Store(true)
	}
	c.enqueue(req)
	resultFormats := []int16{protocol.FormatBinary}

	// Sync fast path for extended query. See simpleQuery for syncMode rationale.
	if c.syncLoop != nil {
		req.syncMode.Store(true)
		c.writerMu.Lock()
		c.writer.Reset()
		if stmtName == "" {
			protocol.AppendParse(c.writer, "", query, nil)
		} else {
			req.extended.SkipParse = true
		}
		protocol.AppendBind(c.writer, "", stmtName, formats, values, resultFormats)
		if hasDescribe {
			protocol.AppendDescribe(c.writer, 'P', "")
		}
		protocol.AppendExecute(c.writer, "", 0)
		protocol.AppendSync(c.writer)
		ok, werr := c.syncLoop.WriteAndPoll(c.fd, c.writer.Bytes(), c.syncBuf, c.onRecv)
		c.writerMu.Unlock()
		if werr != nil {
			c.failReq(req, werr)
			releasePgRequest(req)
			return nil, werr
		}
		if ok && req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			if req.err != nil {
				if len(req.rows) > 0 {
					c.touch()
					return acquirePGRows(req.columns, req.rows, false, req, req.err), nil
				}
				reqErr := req.err
				releasePgRequest(req)
				return nil, reqErr
			}
			c.touch()
			return acquirePGRows(req.columns, req.rows, false, req, nil), nil
		}
		// Clear syncMode so async dispatch can promote to streaming.
		req.syncMode.Store(false)
	} else {
		// Direct-mode extended query. Build the Parse+Bind+[Describe]+
		// Execute+Sync sequence in-place on c.writer to avoid the per-
		// message snapshot copies the Write* helpers return and the
		// joinBytes fusion slice. One owned copy at the end so callers
		// can release writerMu before hitting the wire.
		c.writerMu.Lock()
		c.writer.Reset()
		if stmtName == "" {
			protocol.AppendParse(c.writer, "", query, nil)
		} else {
			req.extended.SkipParse = true
		}
		protocol.AppendBind(c.writer, "", stmtName, formats, values, resultFormats)
		if hasDescribe {
			protocol.AppendDescribe(c.writer, 'P', "")
		}
		protocol.AppendExecute(c.writer, "", 0)
		protocol.AppendSync(c.writer)
		buf := c.writer.Bytes()
		pslot := acquirePayloadBuf(len(buf))
		pslot.buf = append(pslot.buf, buf...)
		c.writerMu.Unlock()
		err := c.writeRaw(pslot.buf)
		releasePayloadBuf(pslot)
		if err != nil {
			c.failReq(req, err)
			releasePgRequest(req)
			return nil, err
		}
	}

	return c.waitForQueryRows(ctx, req, false)
}

func (c *pgConn) extendedExec(ctx context.Context, stmtName, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.doExtendedExec(ctx, stmtName, query, args)
	if stmtName != "" && isPreparedStatementNotFound(err) {
		c.stmtCache.remove(query)
		prep, prepErr := c.prepareStatement(ctx, stmtName, query)
		if prepErr != nil {
			return nil, prepErr
		}
		c.stmtCache.put(query, prep)
		result, err = c.doExtendedExec(ctx, stmtName, query, args)
	}
	return result, err
}

func (c *pgConn) doExtendedExec(ctx context.Context, stmtName, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	values, formats, encSlot, err := encodeArgs(args)
	if err != nil {
		return nil, err
	}
	defer releaseEncodedArgs(encSlot)
	req := acquirePgRequest()
	req.ctx = ctx
	req.kind = reqExtended
	// HasDescribe left false (Exec does not request a row description).
	c.enqueue(req)
	if stmtName != "" {
		req.extended.SkipParse = true
	}

	// Sync fast path for extended exec — build the message using Append
	// variants directly into the writer's buffer, eliminating per-message
	// snapshot + joinBytes allocations. writerMu is held across the
	// WriteAndPoll call so the aliased buffer stays valid.
	if c.syncLoop != nil {
		c.writerMu.Lock()
		c.writer.Reset()
		if stmtName == "" {
			protocol.AppendParse(c.writer, "", query, nil)
		}
		protocol.AppendBind(c.writer, "", stmtName, formats, values, nil)
		protocol.AppendExecute(c.writer, "", 0)
		protocol.AppendSync(c.writer)
		ok, werr := c.syncLoop.WriteAndPoll(c.fd, c.writer.Bytes(), c.syncBuf, c.onRecv)
		c.writerMu.Unlock()
		if werr != nil {
			c.failReq(req, werr)
			releasePgRequest(req)
			return nil, werr
		}
		if ok && req.doneAtom.Load() != 0 {
			select {
			case <-req.doneCh:
			default:
			}
			if req.err != nil {
				reqErr := req.err
				releasePgRequest(req)
				return nil, reqErr
			}
			c.touch()
			tagBytes := req.extended.TagBytes()
			n, _ := protocol.RowsAffectedBytes(tagBytes)
			res := newPGResultFromCount(n)
			releasePgRequest(req)
			return res, nil
		}
	} else {
		// Direct-mode extended exec — same in-place Append strategy as
		// doExtendedQuery to eliminate intermediate snapshot allocs.
		c.writerMu.Lock()
		c.writer.Reset()
		if stmtName == "" {
			protocol.AppendParse(c.writer, "", query, nil)
		}
		protocol.AppendBind(c.writer, "", stmtName, formats, values, nil)
		protocol.AppendExecute(c.writer, "", 0)
		protocol.AppendSync(c.writer)
		buf := c.writer.Bytes()
		pslot := acquirePayloadBuf(len(buf))
		pslot.buf = append(pslot.buf, buf...)
		c.writerMu.Unlock()
		err := c.writeRaw(pslot.buf)
		releasePayloadBuf(pslot)
		if err != nil {
			c.failReq(req, err)
			releasePgRequest(req)
			return nil, err
		}
	}
	if err := c.wait(ctx, req); err != nil {
		releasePgRequest(req)
		return nil, err
	}
	c.touch()
	tagBytes := req.extended.TagBytes()
	n, _ := protocol.RowsAffectedBytes(tagBytes)
	res := newPGResultFromCount(n)
	releasePgRequest(req)
	return res, nil
}

// encodeArgs converts a NamedValue list into Bind-ready ([][]byte, []int16).
// encodedArgsSlot holds the paired []byte+int16 slices that encodeArgs
// returns to protocol.AppendBind. Both are sized to the same length, so
// a single pooled struct amortises two allocations per query.
type encodedArgsSlot struct {
	values  [][]byte
	formats []int16
}

var encodedArgsPool = sync.Pool{
	New: func() any {
		return &encodedArgsSlot{
			values:  make([][]byte, 0, 8),
			formats: make([]int16, 0, 8),
		}
	},
}

// releaseEncodedArgs returns slot to the pool. Callers MUST invoke this
// once protocol.AppendBind has consumed values+formats (synchronously,
// inside the write path).
func releaseEncodedArgs(slot *encodedArgsSlot) {
	if slot == nil {
		return
	}
	// Zero []byte references so encoded payloads aren't pinned in the
	// pool.
	for i := range slot.values {
		slot.values[i] = nil
	}
	slot.values = slot.values[:0]
	slot.formats = slot.formats[:0]
	encodedArgsPool.Put(slot)
}

// payloadBufSlot recycles the []byte snapshot of c.writer.Bytes() taken
// on the direct-mode write path. The slab header lives on the heap once
// (in the pool's New); subsequent Get/Put round-trips reuse the same
// *payloadBufSlot pointer so there is no per-call slice-header alloc.
type payloadBufSlot struct {
	buf []byte
}

var payloadBufPool = sync.Pool{
	New: func() any {
		return &payloadBufSlot{buf: make([]byte, 0, 256)}
	},
}

func acquirePayloadBuf(size int) *payloadBufSlot {
	slot := payloadBufPool.Get().(*payloadBufSlot)
	slot.buf = slot.buf[:0]
	if cap(slot.buf) < size {
		slot.buf = make([]byte, 0, size)
	}
	return slot
}

func releasePayloadBuf(slot *payloadBufSlot) {
	if slot == nil {
		return
	}
	slot.buf = slot.buf[:0]
	payloadBufPool.Put(slot)
}

func encodeArgs(args []driver.NamedValue) ([][]byte, []int16, *encodedArgsSlot, error) {
	if len(args) == 0 {
		return nil, nil, nil, nil
	}
	slot := encodedArgsPool.Get().(*encodedArgsSlot)
	if cap(slot.values) < len(args) {
		slot.values = make([][]byte, len(args))
	} else {
		slot.values = slot.values[:len(args)]
	}
	if cap(slot.formats) < len(args) {
		slot.formats = make([]int16, len(args))
	} else {
		slot.formats = slot.formats[:len(args)]
	}
	for i, a := range args {
		b, fmtCode, err := encodeOne(a.Value)
		if err != nil {
			releaseEncodedArgs(slot)
			return nil, nil, nil, fmt.Errorf("celeris-postgres: encode arg %d: %w", i+1, err)
		}
		slot.values[i] = b
		slot.formats[i] = fmtCode
	}
	return slot.values, slot.formats, slot, nil
}

// encodeOne picks a binary encoding for common Go types and falls back to
// text for the rest. PG accepts per-parameter format codes in Bind.
func encodeOne(v any) ([]byte, int16, error) {
	if v == nil {
		return nil, protocol.FormatBinary, nil
	}
	switch x := v.(type) {
	case bool:
		return []byte{boolByte(x)}, protocol.FormatBinary, nil
	case int:
		// Text format so PG coerces to the parameter's actual integer type
		// (int2/int4/int8). Binary requires the exact byte width to match
		// the server-side OID, which we don't know in encodeOne.
		return strconv.AppendInt(nil, int64(x), 10), protocol.FormatText, nil
	case int32:
		return strconv.AppendInt(nil, int64(x), 10), protocol.FormatText, nil
	case int64:
		return strconv.AppendInt(nil, x, 10), protocol.FormatText, nil
	case float32:
		return strconv.AppendFloat(nil, float64(x), 'f', -1, 32), protocol.FormatText, nil
	case float64:
		return strconv.AppendFloat(nil, x, 'f', -1, 64), protocol.FormatText, nil
	case string:
		return []byte(x), protocol.FormatText, nil
	case []byte:
		return x, protocol.FormatBinary, nil
	case time.Time:
		// Text format handles timestamptz, timestamp, and date columns
		// without requiring knowledge of the server-side parameter OID.
		return []byte(x.Format(time.RFC3339Nano)), protocol.FormatText, nil
	default:
		// Previously: fmt.Sprint(v) was wired through as text. That silently
		// produces garbage (e.g. "{1 2}") for struct types and anything else
		// whose default fmt form is not a valid PG literal. Callers must
		// convert to a supported type (bool, int64, float64, string,
		// []byte, time.Time) or register a codec in the protocol package.
		return nil, 0, fmt.Errorf("celeris-postgres: unsupported argument type %T; convert to bool/int64/float64/string/[]byte/time.Time or register a codec", v)
	}
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// -------------------- driver.Conn misc ----------------------------------

// Close releases the conn's FD and (if it owns the event loop) releases it.
//
// FD-close contract: the event loop's UnregisterConn does NOT close the fd;
// the driver owns it. We route syscall.Close through closeFDOnce so that if
// onClose (fired by the event loop on peer EOF) closes the fd first, Close()
// does not double-close. Without this guard, a worker could reuse the fd
// number between UnregisterConn and syscall.Close — the classic phantom-
// socket bug.
func (c *pgConn) Close() error {
	var firstErr error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		termBytes := c.buildMessage(func(w *protocol.Writer) []byte {
			w.Reset()
			w.StartMessage(protocol.MsgTerminate)
			w.FinishMessage()
			return append([]byte(nil), w.Bytes()...)
		})
		if c.useDirect {
			if c.tcp != nil {
				_ = c.tcp.SetWriteDeadline(time.Now().Add(time.Second))
				if err := c.writeRaw(termBytes); err != nil && firstErr == nil {
					firstErr = err
				}
				if err := c.tcp.Close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		} else if c.loop != nil {
			if err := c.writeRaw(termBytes); err != nil && firstErr == nil {
				firstErr = err
			}
			if err := c.loop.UnregisterConn(c.fd); err != nil && firstErr == nil {
				firstErr = err
			}
			c.closeFDOnce()
		}
		c.failAll(ErrClosed)
		if c.closeLoop != nil {
			c.closeLoop()
		}
	})
	// Join any background goroutines (dropPreparedAsync) spawned by
	// this conn. Safe outside closeOnce because closeWG.Wait() on
	// a zero WaitGroup returns immediately.
	c.closeWG.Wait()
	return firstErr
}

// Begin implements driver.Conn.
func (c *pgConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx satisfies driver.ConnBeginTx.
func (c *pgConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.sessionDirty.Store(true)
	sql := buildBeginSQL(opts)
	if err := c.simpleExecNoTag(ctx, sql); err != nil {
		return nil, err
	}
	return &pgTx{conn: c}, nil
}

// buildBeginSQL turns driver.TxOptions into a BEGIN statement that carries
// the isolation level and read-only flag in a single round trip. The
// numeric values match database/sql.IsolationLevel.
func buildBeginSQL(opts driver.TxOptions) string {
	var b strings.Builder
	b.WriteString("BEGIN")
	if opts.ReadOnly {
		b.WriteString(" READ ONLY")
	}
	switch opts.Isolation {
	case 0:
	case 1:
		b.WriteString(" ISOLATION LEVEL READ UNCOMMITTED")
	case 2:
		b.WriteString(" ISOLATION LEVEL READ COMMITTED")
	case 3, 4:
		b.WriteString(" ISOLATION LEVEL REPEATABLE READ")
	case 5, 6, 7, 8:
		b.WriteString(" ISOLATION LEVEL SERIALIZABLE")
	}
	return b.String()
}

// Ping implements driver.Pinger. It issues SELECT 1 via the simple protocol.
func (c *pgConn) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	return c.simpleExecNoTag(ctx, "SELECT 1")
}

// ResetSession implements driver.SessionResetter. It is invoked by
// database/sql when a conn is returned to the idle pool. We send DISCARD ALL
// so the next caller starts with a clean session (temp tables dropped,
// cursors closed, prepared statements deallocated, GUC settings reset).
// If the reset fails we return ErrBadConn so the pool evicts and reopens.
//
// Because server-side prepared statements are destroyed by DISCARD ALL, we
// also reset the client-side LRU so later PrepareContext calls re-Parse
// rather than hitting a stale cached name that the server has forgotten.
func (c *pgConn) ResetSession(ctx context.Context) error {
	if c.closed.Load() {
		return ErrBadConn
	}
	dirty := c.sessionDirty.Swap(false)
	// Fast path: if the session is clean (only simple queries ran, no
	// transactions, no SET commands), skip the expensive round trip entirely.
	// Prepared statements in the LRU cache are intentionally kept alive — the
	// re-prepare-on-miss code in extendedExec/extendedQuery handles the case
	// where they're unexpectedly dropped (e.g. by a DISCARD ALL after a
	// transaction). This saves ~90µs per conn-return for the common workload.
	if !dirty {
		return nil
	}
	// Session truly dirty (BEGIN, SET, temp tables, etc.) — full reset.
	// Bound the reset to 1s so a stuck server doesn't pin the caller.
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := c.simpleExecNoTag(cctx, "DISCARD ALL"); err != nil {
		return ErrBadConn
	}
	c.stmtCache.reset()
	return nil
}

// IsValid implements driver.Validator.
func (c *pgConn) IsValid() bool {
	return !c.closed.Load()
}

// Worker satisfies async.Conn.
func (c *pgConn) Worker() int { return c.workerID }

// IsExpired reports whether the conn has exceeded MaxLifetime.
func (c *pgConn) IsExpired(now time.Time) bool {
	if c.maxLifetime <= 0 {
		return false
	}
	return now.Sub(c.createdAt) >= c.maxLifetime
}

// IsIdleTooLong reports whether the conn has been idle longer than MaxIdleTime.
func (c *pgConn) IsIdleTooLong(now time.Time) bool {
	if c.maxIdleTime <= 0 {
		return false
	}
	last := time.Unix(0, c.lastUsedAt.Load())
	return now.Sub(last) >= c.maxIdleTime
}

func (c *pgConn) touch() {
	c.lastUsedAt.Store(time.Now().UnixNano())
}

// Savepoint issues SAVEPOINT <name> on this conn. Reach via sql.Conn.Raw.
// name must match [A-Za-z0-9_]+ — this avoids SQL injection without having
// to parse the PG identifier grammar. If you need a savepoint with other
// characters, issue a raw simple query instead.
func (c *pgConn) Savepoint(ctx context.Context, name string) error {
	return c.savepointCmd(ctx, "SAVEPOINT", name)
}

// ReleaseSavepoint issues RELEASE SAVEPOINT <name>. Same name rules as
// Savepoint.
func (c *pgConn) ReleaseSavepoint(ctx context.Context, name string) error {
	return c.savepointCmd(ctx, "RELEASE SAVEPOINT", name)
}

// RollbackToSavepoint issues ROLLBACK TO SAVEPOINT <name>. Same name rules
// as Savepoint.
func (c *pgConn) RollbackToSavepoint(ctx context.Context, name string) error {
	return c.savepointCmd(ctx, "ROLLBACK TO SAVEPOINT", name)
}

func quoteIdent(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' {
			b.WriteString(`""`)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// joinBytes concatenates byte slices into one owned buffer so callers can
// issue a single loop.Write and avoid per-message kernel round trips.
func joinBytes(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ServerParam returns a ParameterStatus value reported by the server. Thread-
// safe: guards against concurrent ParameterStatus handling on the event loop.
func (c *pgConn) ServerParam(name string) string {
	c.serverParamsMu.RLock()
	defer c.serverParamsMu.RUnlock()
	if c.serverParams == nil {
		return ""
	}
	return c.serverParams[name]
}

// CheckNamedValue lets us accept any Go type and let encodeOne deal with it.
func (c *pgConn) CheckNamedValue(_ *driver.NamedValue) error {
	return nil
}

// copyFrom streams rows from src into tableName(columns) via COPY FROM STDIN.
// It returns the row count parsed from the server's CommandComplete tag.
//
// Contract with the event loop:
//   - We enqueue a reqCopyIn request and send the simple-query that starts
//     COPY. The server replies CopyInResponse (triggering copyReady) or
//     ErrorResponse + ReadyForQuery (triggering doneCh).
//   - We select on copyReady / doneCh / ctx.Done. Once copyReady fires, we
//     stream CopyData frames through loop.Write, then send CopyDone (or
//     CopyFail on iteration error) and wait for doneCh.
//   - Any write error aborts the copy; we attempt CopyFail so the server's
//     FSM can recover. If CopyFail itself fails, the conn is effectively
//     dead and the pool will discard it on the next use.
func (c *pgConn) copyFrom(ctx context.Context, tableName string, columns []string, src CopyFromSource) (int64, error) {
	if c.closed.Load() {
		return 0, ErrClosed
	}
	if tableName == "" {
		return 0, errors.New("celeris-postgres: CopyFrom requires a table name")
	}
	if c.useDirect {
		// Direct mode has no event-loop goroutine driving onRecv for us.
		// Spawn a short-lived reader that pumps tcp.Read -> onRecv for
		// the lifetime of this copy. copyReady/doneCh fire from the
		// reader's onRecv calls; the caller goroutine remains the sole
		// writer of CopyData frames (tcp.Write is safe alongside
		// concurrent tcp.Read on a different goroutine).
		stop := c.startDirectReader()
		defer stop()
	}
	query := buildCopyFromQuery(tableName, columns)
	req := &pgRequest{
		ctx:       ctx,
		kind:      reqCopyIn,
		copyIn:    &protocol.CopyInState{},
		doneCh:    make(chan struct{}),
		copyReady: make(chan struct{}),
	}
	c.enqueue(req)
	payload := c.buildMessage(func(w *protocol.Writer) []byte { return protocol.WriteQuery(w, query) })
	if err := c.writeRaw(payload); err != nil {
		c.failReq(req, err)
		return 0, err
	}
	// Wait for CopyInResponse before streaming.
	select {
	case <-req.copyReady:
	case <-req.doneCh:
		// Server errored before CopyInResponse — surface the error.
		if req.err != nil {
			return 0, req.err
		}
		return 0, errors.New("celeris-postgres: COPY FROM ended before CopyInResponse")
	case <-ctx.Done():
		if c.pid != 0 && c.secret != 0 {
			_ = sendCancelRequest(ctx, c.addr, c.pid, c.secret)
		}
		<-req.doneCh
		if req.err != nil {
			return 0, req.err
		}
		return 0, ctx.Err()
	}

	// Stream rows. We reuse a scratch buffer per call to hold each encoded
	// row so we don't allocate a fresh slice in the hot loop. Each CopyData
	// wrapper still allocates a small framed-message copy (WriteCopyData).
	// awaitDoneBounded waits for req.doneCh with a 30s ceiling — a
	// dead reader goroutine in direct mode would otherwise wedge
	// the caller forever.
	awaitDoneBounded := func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-req.doneCh:
		case <-timer.C:
			c.failAll(errors.New("celeris-postgres: COPY cleanup wait timeout"))
		}
	}
	var rowBuf []byte
	for src.Next() {
		vals, verr := src.Values()
		if verr != nil {
			_ = c.sendCopyFail(verr.Error())
			awaitDoneBounded()
			return 0, verr
		}
		rowBuf = encodeTextRow(rowBuf[:0], vals)
		frame := c.buildMessage(func(w *protocol.Writer) []byte { return protocol.WriteCopyData(w, rowBuf) })
		if werr := c.writeRaw(frame); werr != nil {
			_ = c.sendCopyFail(werr.Error())
			awaitDoneBounded()
			return 0, werr
		}
	}
	if serr := src.Err(); serr != nil {
		_ = c.sendCopyFail(serr.Error())
		awaitDoneBounded()
		return 0, serr
	}

	// End with CopyDone. The server then sends CommandComplete + ReadyForQuery
	// which closes req.doneCh.
	doneFrame := c.buildMessage(func(w *protocol.Writer) []byte { return protocol.WriteCopyDone(w) })
	if werr := c.writeRaw(doneFrame); werr != nil {
		awaitDoneBounded()
		return 0, werr
	}
	if c.useDirect {
		// Direct mode: the background reader goroutine drives onRecv
		// which fires doneCh. c.wait would spawn a second tcp reader —
		// bad. Handle ctx cancellation explicitly: send CancelRequest
		// and wait bounded for the server's Error+RFQ to complete
		// req. Without this, a canceled COPY leaves req in the pending
		// queue and desynchronizes the wire on the next query (#241).
		if err := c.awaitDirectWithCancel(ctx, req); err != nil {
			return 0, err
		}
	} else {
		if err := c.wait(ctx, req); err != nil {
			return 0, err
		}
	}
	c.touch()
	n, _ := protocol.RowsAffected(req.tag)
	return n, nil
}

// awaitDirectWithCancel waits for req.doneCh in direct mode, with
// proper ctx.Done handling: CancelRequest + bounded wait for the
// server's Error+RFQ to drain the pipeline. On timeout, fails the
// whole conn so the pool discards it — avoids leaving an orphaned
// request in the pending queue.
func (c *pgConn) awaitDirectWithCancel(ctx context.Context, req *pgRequest) error {
	select {
	case <-req.doneCh:
		return req.err
	case <-ctx.Done():
		if c.pid != 0 && c.secret != 0 {
			_ = sendCancelRequest(ctx, c.addr, c.pid, c.secret)
		}
		// Wait for the reader goroutine to observe Error+RFQ and
		// complete req. Bounded so a wedged server can't pin the
		// caller forever.
		drainTimer := time.NewTimer(30 * time.Second)
		defer drainTimer.Stop()
		select {
		case <-req.doneCh:
			return ctx.Err()
		case <-drainTimer.C:
			c.failAll(errors.New("celeris-postgres: cancel timeout draining direct-mode req"))
			return ctx.Err()
		}
	}
}

// sendCopyFail writes a CopyFail frame; errors from the write are advisory —
// the caller is already unwinding an error path.
func (c *pgConn) sendCopyFail(reason string) error {
	frame := c.buildMessage(func(w *protocol.Writer) []byte { return protocol.WriteCopyFail(w, reason) })
	return c.writeRaw(frame)
}

// copyTo issues COPY (<query>) TO STDOUT and invokes dest for every row.
// Each row slice is freshly allocated (not aliased) so dest may retain it.
func (c *pgConn) copyTo(ctx context.Context, query string, dest func(row []byte) error) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if dest == nil {
		return errors.New("celeris-postgres: CopyTo requires a non-nil dest")
	}
	if c.useDirect {
		stop := c.startDirectReader()
		defer stop()
	}
	req := &pgRequest{
		ctx:       ctx,
		kind:      reqCopyOut,
		copyOut:   &protocol.CopyOutState{},
		doneCh:    make(chan struct{}),
		onCopyRow: dest,
	}
	c.enqueue(req)
	payload := c.buildMessage(func(w *protocol.Writer) []byte { return protocol.WriteQuery(w, query) })
	if err := c.writeRaw(payload); err != nil {
		c.failReq(req, err)
		return err
	}
	if c.useDirect {
		// See awaitDirectWithCancel contract: CancelRequest + bounded
		// drain so ctx.Done doesn't leave req orphaned (#241).
		if err := c.awaitDirectWithCancel(ctx, req); err != nil {
			return err
		}
	} else {
		if err := c.wait(ctx, req); err != nil {
			return err
		}
	}
	c.touch()
	return nil
}

// buildCopyFromQuery assembles a COPY ... FROM STDIN statement. Column names
// and the table name are quoted to protect against injection via identifiers
// that happen to contain quote characters.
func buildCopyFromQuery(table string, columns []string) string {
	var b strings.Builder
	b.WriteString("COPY ")
	b.WriteString(quoteIdent(table))
	if len(columns) > 0 {
		b.WriteString(" (")
		for i, col := range columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteIdent(col))
		}
		b.WriteByte(')')
	}
	b.WriteString(" FROM STDIN WITH (FORMAT text)")
	return b.String()
}

// Savepoint issues SAVEPOINT <name> on this conn. Reach via sql.Conn.Raw.
func (c *pgConn) savepointCmd(ctx context.Context, verb, name string) error {
	if err := validateSavepointName(name); err != nil {
		return err
	}
	return c.simpleExecNoTag(ctx, verb+" "+name)
}

// validateSavepointName rejects names containing characters outside
// [A-Za-z0-9_]. This is the conservative superset of identifiers that need
// no quoting; anything outside is a likely SQL-injection attempt.
func validateSavepointName(name string) error {
	if name == "" {
		return errors.New("celeris-postgres: savepoint name is empty")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			return fmt.Errorf("celeris-postgres: invalid savepoint name %q", name)
		}
	}
	return nil
}

// Compile-time interface assertions.
var (
	_ driver.Conn               = (*pgConn)(nil)
	_ driver.ConnBeginTx        = (*pgConn)(nil)
	_ driver.ConnPrepareContext = (*pgConn)(nil)
	_ driver.QueryerContext     = (*pgConn)(nil)
	_ driver.ExecerContext      = (*pgConn)(nil)
	_ driver.Pinger             = (*pgConn)(nil)
	_ driver.SessionResetter    = (*pgConn)(nil)
	_ driver.Validator          = (*pgConn)(nil)
	_ driver.NamedValueChecker  = (*pgConn)(nil)
	_ async.Conn                = (*pgConn)(nil)
)
