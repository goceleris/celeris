//go:build linux

package iouring

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#657, face 2: an io_uring hand-off must never leave a read that can
// still resolve the descriptor. These helpers drive one connection through
// the real request, send and hand-off code with the kernel taken out of the
// loop: every SQE the engine places is read back and removed from the SQ ring
// unsubmitted, and every completion is written by the test. That makes each
// ordering the kernel can produce (a cancel that hits, one that misses, data
// that beats the cancel) a deterministic, repeatable input.

// wantReapTag is the op tag of the hand-off's own recv cancel (DECISION P2:
// udTransplantReap = 0x09<<56, a tag of its own so its completion can be told
// apart from the WebSocket pause's cancel, celeris#596). Spelled as a literal
// so this file compiles on a tree that does not have the constant yet.
const wantReapTag uint64 = 0x09 << 56

// sqeRec is one SQE as the kernel would read it.
type sqeRec struct {
	op    uint8
	flags uint8
	fd    int32
	addr  uint64
	ud    uint64
}

func (r sqeRec) tag() uint64 { return r.ud & udMask }

func (r sqeRec) String() string {
	name := map[uint8]string{opSEND: "SEND", opRECV: "RECV", opASYNCCANCEL: "ASYNC_CANCEL", opWRITEV: "WRITEV",
		opTIMEOUT: "TIMEOUT"}[r.op]
	if name == "" {
		name = "op" + strconv.Itoa(int(r.op))
	}
	return name + "{flags:" + strconv.Itoa(int(r.flags)) + " tag:0x" + strconv.FormatUint(r.tag()>>56, 16) +
		" fd:" + strconv.Itoa(int(r.fd)) + "}"
}

// takeSQEs returns every SQE placed on r since the last call, in placement
// order, and removes them from the SQ ring without submitting them: the kernel
// never sees them, so every completion in these tests is the test's own.
func takeSQEs(r *Ring) []sqeRec {
	head := atomic.LoadUint32((*uint32)(r.sqHead))
	tail := atomic.LoadUint32((*uint32)(r.sqTail))
	var out []sqeRec
	for i := head; i != tail; i++ {
		b := r.sqes[uintptr(i&r.sqMask)*sqeSize:]
		out = append(out, sqeRec{
			op:    b[0],
			flags: b[1],
			fd:    *(*int32)(unsafe.Pointer(&b[4])),
			addr:  *(*uint64)(unsafe.Pointer(&b[16])),
			ud:    *(*uint64)(unsafe.Pointer(&b[32])),
		})
	}
	atomic.StoreUint32((*uint32)(r.sqTail), head)
	r.pending = 0
	return out
}

// fdlHandler answers "ok", or a body large enough for the scatter-gather
// (WRITEV) path on /large.
type fdlHandler struct{}

var fdlLargeBody = make([]byte, 16<<10)

func (fdlHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/large" {
		body = fdlLargeBody
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

const fdlGET = "GET / HTTP/1.1\r\nHost: x\r\n\r\n"

// fdlTarget stands in for the epoll engine on the receiving end of a hand-off.
// It closes what it adopts, or, with refuse set, refuses it the way a target
// that cannot take the fd does (the source then reclaims it).
type fdlTarget struct {
	adopted atomic.Int64
	refuse  bool
}

func (r *fdlTarget) AdoptConn(fd int, _ engine.Carryover) error {
	if r.refuse {
		return errors.New("fdlTarget: refused")
	}
	r.adopted.Add(1)
	_ = unix.Close(fd)
	return nil
}

// fdlFixture is one HTTP/1 keep-alive connection on a ring-backed Worker, set
// up the way onAcceptedFD sets up a real one. e is the engine the worker's
// celeris#657 counters belong to, so tests read them through Metrics() —
// the path the validation artifact reads.
type fdlFixture struct {
	t        *testing.T
	e        *Engine
	w        *Worker
	cs       *connState
	fd, peer int
	gen      uint32
	tgt      *fdlTarget
	now      int64
}

func newFDLFixture(t *testing.T, async bool) *fdlFixture {
	t.Helper()
	fd, peer := socketPairFDs(t)
	_ = unix.SetNonblock(fd, true)
	e := &Engine{}
	w := newLedgerWorker(fd)
	// Room for the descriptor a refused hand-off is reclaimed on.
	if len(w.conns) < 1024 {
		w.conns = make([]*connState, 1024)
	}
	w.ring = newTestRing(t)
	w.handler = fdlHandler{}
	w.cfg = resource.Config{Protocol: engine.HTTP1, MaxRequestBodySize: 1 << 20}
	w.resolved.BufferSize = 4096
	w.reqCount = new(atomic.Uint64)
	w.liveConns = make([]int, 0, 4)
	w.async = async
	// The engine-wide counters createWorkers wires, taken from e so a
	// Metrics() read sees them.
	w.recvArm = &e.metrics.recvArm
	w.handoffLoss = &e.metrics.handoffLoss
	w.transplantDetached = &e.metrics.transplantDetached
	w.transplantHandoffRefused = &e.metrics.transplantHandoffRefused
	w.transplantAdoptRefused = &e.metrics.transplantAdoptRefused
	// createWorkers copies the engine's async-cancel-flags probe answer; the
	// kernel never sees this fixture's SQEs, so it reads "accepted", the
	// answer of every kernel from 5.19. A tree without the probe reaps
	// unconditionally, which is the same thing.
	trySetWorkerField(w, "asyncCancelFlags", true)

	cs := acquireConnState(context.Background(), fd, 4096, async)
	cs.writeFn = w.makeWriteFn(cs)
	cs.protocol.Store(int32(engine.HTTP1))
	cs.detected = true
	w.initProtocol(cs)
	w.conns[fd] = cs
	w.connCount = 1
	w.addLiveConn(cs)
	w.maxFD = fd
	w.activeConns.Add(1)
	f := &fdlFixture{t: t, e: e, w: w, cs: cs, fd: fd, peer: peer, gen: cs.generation, tgt: &fdlTarget{}}
	t.Cleanup(func() {
		_ = unix.Close(peer)
		// Every descriptor the worker still owns (the fixture's own, or the
		// one a refused hand-off was reclaimed on): nothing else closes them.
		for _, c := range w.conns {
			if c != nil {
				_ = unix.Close(c.fd)
			}
		}
	})
	return f
}

// startDrain / stopDrain are Engine.StartTransplant / StopTransplant for this
// one worker. A new holder each time, as StartTransplant makes.
func (f *fdlFixture) startDrain() {
	f.w.transplant.Store(&transplantTargetHolder{target: f.tgt})
}

func (f *fdlFixture) stopDrain() { f.w.transplant.Store(nil) }

// armFirstRecv is onAcceptedFD's first recv arm.
func (f *fdlFixture) armFirstRecv() {
	f.t.Helper()
	if !f.w.prepareRecv(f.cs, f.cs.buf) {
		f.t.Fatal("first recv arm refused on an empty ring")
	}
	if got := takeSQEs(f.w.ring); len(got) != 1 || got[0].op != opRECV {
		f.t.Fatalf("first arm placed %v, want one RECV", got)
	}
}

func (f *fdlFixture) process(c *completionEntry) {
	f.now++
	f.w.processCQE(context.Background(), c, f.now)
}

func (f *fdlFixture) recvCQE(res int32) *completionEntry {
	return &completionEntry{UserData: encodeUserDataGen(udRecv, f.fd, f.gen), Res: res}
}

// reapCQE is the completion of the hand-off's own recv cancel.
func (f *fdlFixture) reapCQE(res int32) *completionEntry {
	return &completionEntry{UserData: encodeUserDataGen(wantReapTag, f.fd, f.gen), Res: res}
}

// sendCQE completes the SEND in flight in full.
func (f *fdlFixture) sendCQE() *completionEntry {
	f.t.Helper()
	if !f.cs.sending {
		f.t.Fatal("sendCQE: no SEND in flight")
	}
	n := len(f.cs.sendBuf) + len(f.cs.sendBody)
	return &completionEntry{UserData: encodeUserDataGen(udSend, f.fd, f.gen), Res: int32(n)}
}

// deliver completes the armed recv with req's bytes in cs.buf.
func (f *fdlFixture) deliver(req string) {
	n := copy(f.cs.buf, req)
	f.process(f.recvCQE(int32(n)))
}

// serveOne runs one request to completion with no drain set, leaving the
// state every keep-alive conn is in between requests on the base: the
// response flushed and its linked RECV armed.
func (f *fdlFixture) serveOne() {
	f.t.Helper()
	f.deliver(fdlGET)
	if got := takeSQEs(f.w.ring); len(got) != 2 || got[0].op != opSEND || got[1].op != opRECV {
		f.t.Fatalf("serving a request with no drain placed %v, want SEND then its linked RECV", got)
	}
	f.process(f.sendCQE())
	if got := takeSQEs(f.w.ring); len(got) != 0 {
		f.t.Fatalf("the SEND's completion placed %v, want nothing", got)
	}
	if !f.cs.recvArmed || f.cs.sending {
		f.t.Fatalf("after one request: recvArmed=%v sending=%v, want true/false", f.cs.recvArmed, f.cs.sending)
	}
}

// metric reads one EngineMetrics field by name, so a test can pin a counter
// that a tree may not have yet: a missing field fails the test by name
// instead of failing the package build.
func metric(t *testing.T, e *Engine, name string) uint64 {
	t.Helper()
	v := reflect.ValueOf(e.Metrics()).FieldByName(name)
	if !v.IsValid() {
		t.Fatalf("engine.EngineMetrics has no field %s (celeris#657 PR-2 counter)", name)
	}
	return v.Uint()
}

// trySetWorkerField sets w's field name to v and reports whether w has such a
// field. With metric, it lets a test pin a field a tree may not have yet: the
// test then fails on its assertions, instead of the package failing to build.
func trySetWorkerField(w *Worker, name string, v any) bool {
	f := reflect.ValueOf(w).Elem().FieldByName(name)
	if !f.IsValid() {
		return false
	}
	reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Set(reflect.ValueOf(v))
	return true
}

// isReap reports whether s is the hand-off's reported cancel of cs's recv:
// matched on the recv's own user_data (op, fd AND generation, so it can never
// cancel the next owner of the fd number), tagged with the reap tag, and
// REPORTED (no CQE_SKIP_SUCCESS), so a miss produces a completion.
func (f *fdlFixture) isReap(s sqeRec) bool {
	return s.op == opASYNCCANCEL && s.flags&sqeCQESkipSuccess == 0 &&
		s.addr == encodeUserDataGen(udRecv, f.fd, f.gen) &&
		s.ud == encodeUserDataGen(wantReapTag, f.fd, f.gen)
}

func fdIsOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// goidHandler is fdlHandler recording the goroutine each request ran on. A
// promoted async conn runs its requests on its dispatch goroutine, so a
// goroutine that exits at its park and is respawned by the next request shows
// up as a new id (celeris#681 R1).
type goidHandler struct {
	mu  sync.Mutex
	ids []uint64
}

func (h *goidHandler) HandleStream(ctx context.Context, s *stream.Stream) error {
	h.mu.Lock()
	h.ids = append(h.ids, goroutineID())
	h.mu.Unlock()
	return fdlHandler{}.HandleStream(ctx, s)
}

func (h *goidHandler) served() []uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uint64(nil), h.ids...)
}

// goroutineID is the running goroutine's id, from its stack header
// ("goroutine 18 [running]:").
func goroutineID() uint64 {
	var b [64]byte
	s := strings.TrimPrefix(string(b[:runtime.Stack(b[:], false)]), "goroutine ")
	id, _ := strconv.ParseUint(s[:strings.IndexByte(s, ' ')], 10, 64)
	return id
}

// goroutineWaitsOnCond reports whether goroutine id is blocked in
// sync.Cond.Wait, which is where a dispatch goroutine parks.
func goroutineWaitsOnCond(id uint64) bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	return regexp.MustCompile(`(?m)^goroutine ` + strconv.FormatUint(id, 10) + ` \[sync\.Cond\.Wait`).Match(buf)
}

// asyncParkFixture is fdlFixture's conn promoted to async dispatch and served
// by h, on a worker whose asyncCancelFlags is flags, with its first recv armed
// and a drain set: every request delivered now runs on the conn's dispatch
// goroutine, which writes the response itself and then reaches its park. The
// cleanup ends that goroutine and waits for it before the fixture closes the
// descriptors.
func asyncParkFixture(t *testing.T, flags bool) (*fdlFixture, *goidHandler) {
	t.Helper()
	f := newFDLFixture(t, true)
	h := &goidHandler{}
	f.w.handler = h
	trySetWorkerField(f.w, "asyncCancelFlags", flags)
	f.cs.asyncPromoted.Store(true)
	f.armFirstRecv()
	f.startDrain()
	t.Cleanup(func() {
		f.cs.asyncInMu.Lock()
		f.cs.asyncClosed.Store(true)
		f.cs.asyncCond.Broadcast()
		f.cs.asyncInMu.Unlock()
		f.w.asyncWG.Wait()
	})
	return f, h
}

// asyncRequest delivers one request to f's promoted conn, reads its response
// at the client, and waits until the dispatch goroutine that answered it has
// reached its park: there it either claimed its own hand-off
// (transplantPending, enqueued for the worker, and exited) or waits on its
// cond for the next request. It reports which, and the SQEs the delivery
// placed.
func (f *fdlFixture) asyncRequest(h *goidHandler) (claimed bool, placed []sqeRec) {
	f.t.Helper()
	n := len(h.served())
	f.deliver(fdlGET)
	placed = takeSQEs(f.w.ring)
	fds := []unix.PollFd{{Fd: int32(f.peer), Events: unix.POLLIN}}
	if k, err := unix.Poll(fds, 10000); err != nil || k != 1 {
		f.t.Fatalf("no response at the client within 10s (poll %d, %v)", k, err)
	}
	var resp [512]byte
	if k, err := unix.Read(f.peer, resp[:]); err != nil || !strings.HasPrefix(string(resp[:max(k, 0)]), "HTTP/1.1 200") {
		f.t.Fatalf("client read %q, %v: want a 200 response", resp[:max(k, 0)], err)
	}
	ids := h.served()
	if len(ids) != n+1 {
		f.t.Fatalf("the handler ran %d times for one request", len(ids)-n)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if f.cs.transplantPending.Load() || f.w.detachQPending.Load() != 0 {
			return true, placed
		}
		if goroutineWaitsOnCond(ids[n]) {
			return false, placed
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the dispatch goroutine neither claimed its hand-off nor parked within 10s of its response")
		}
	}
}

// distinct counts the distinct values in ids.
func distinct(ids []uint64) int {
	seen := map[uint64]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	return len(seen)
}
