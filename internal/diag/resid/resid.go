// Package resid is the server-residence instrument of celeris#757 (DESIGN-757
// r2, section 2). THROWAWAY DIAGNOSTIC: it lives on the diag/757-residence-stamps
// branch only, is never merged and never part of a release.
//
// It measures where the time of an HTTP/2 round trip goes on the server: from
// the kernel's receive of the request to the return of the write(2) that sends
// the response. The epoll engine stamps a sample of its reads (1 in 64) and
// records the segments into fixed-size log2 histograms, one set per loop, that
// the loop's own thread writes and a 1 Hz reader sums:
//
//	R   T_wr   - T_rx    request read to response write returned (the residence)
//	G   T_rx   - T_krx   kernel receive of the last skb to the read returning
//	S1  T_rx   - T_ew    epoll_wait return to the read (batch position)
//	S2  T_proc - T_rx    ProcessH2: the handler, inline on the loop
//	S3  T_wr   - T_proc  the flush: the write(2)
//	B   per sampled loop turn: epoll_wait return to the end of the batch
//	Wt  per sampled loop turn: time blocked in epoll_wait
//
// R, G, S1, S2 and S3 are weighted by k, the requests the sampled read carried.
//
// # Gate
//
// The environment variable [EnvGate] (CELERIS_DIAG_RESID) is read once, at
// process start. Unset (or "", "0", "false", "off", "no") nothing is recorded,
// [NewLoop] returns nil and every site in the engine is one nil check on a
// loop-local pointer: the build is the same as main's.
//
// # Export
//
// [Snapshot] sums the loops (cumulative since start) and [Snap.Line]
// formats it as the `[res]` line the probatorium sampler prints beside
// `[sched]`; [Parse] reads one back. See [Snap.Line] for the format.
package resid

import (
	"math/bits"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// EnvGate is the environment variable that turns the instrument on.
const EnvGate = "CELERIS_DIAG_RESID"

// SampleShift is the sampling rate: 1 recv in 1<<SampleShift is sampled (DESIGN-757 r2: 1 in 64).
const SampleShift = 6

// The histograms.
const (
	HistR = iota
	HistG
	HistS1
	HistS2
	HistS3
	HistB
	HistWt
	NumHists
)

// HistNames are the keys of the histograms in the `[res]` line, by index.
var HistNames = [NumHists]string{"R", "G", "S1", "S2", "S3", "B", "Wt"}

// NumBuckets is the size of a histogram: 4 linear buckets for 0..3 ns, then 4
// sub-buckets per octave up to 2^40 ns (about 18 minutes); the last bucket
// also takes everything above (the overflow).
const NumBuckets = 160

// BucketOf returns the bucket of a value in nanoseconds (negative is 0).
func BucketOf(v int64) int {
	if v < 4 {
		if v < 0 {
			return 0
		}
		return int(v)
	}
	e := bits.Len64(uint64(v)) - 1 // v in [2^e, 2^(e+1)), e >= 2
	i := (e-1)*4 + int(uint64(v)>>(e-2))&3
	if i >= NumBuckets {
		return NumBuckets - 1
	}
	return i
}

// BucketLo returns the lower bound, in nanoseconds, of bucket i.
func BucketLo(i int) int64 {
	if i < 4 {
		return int64(i)
	}
	e := i/4 + 1
	return int64(4+i%4) << (e - 2)
}

// Hist is a lock-free log2 histogram with a running sum. One writer (the loop's
// thread) and any number of readers; every word is an atomic.
type Hist struct {
	sum atomic.Uint64 // sum of value*weight, ns
	b   [NumBuckets]atomic.Uint64
}

// Add records value v (ns) with weight w.
func (h *Hist) Add(v int64, w uint64) {
	if v < 0 {
		v = 0
	}
	h.b[BucketOf(v)].Add(w)
	h.sum.Add(uint64(v) * w)
}

func (h *Hist) snap() HistSnap {
	var s HistSnap
	for i := range h.b {
		c := h.b[i].Load()
		s.B[i] = c
		s.N += c
	}
	s.SumNS = h.sum.Load()
	return s
}

// Loop is the state of one event loop. Everything but the atomics is touched by
// the loop's thread only.
type Loop struct {
	id      int
	rng     uint64 // recv sampler (xorshift64*)
	turnRng uint64 // loop-turn sampler
	shift   uint   // 1 in 1<<shift

	N, Unst, Part, NoWr, Early, InProc atomic.Uint64
	H                                  [NumHists]Hist
	// Loops sharing a cache line with a neighbour's counters would make the
	// "per-loop, no contention" claim false.
	_ [64]byte
}

var (
	enabled atomic.Bool

	regMu sync.Mutex
	reg   []*Loop
)

func init() { enabled.Store(gateOn(os.Getenv(EnvGate))) }

func gateOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// Enabled reports whether the gate is on.
func Enabled() bool { return enabled.Load() }

// SetEnabled turns the gate on or off and returns the function that restores
// it. For tests: it must be called before the engine is created, a loop reads
// the gate once.
func SetEnabled(on bool) (restore func()) {
	prev := enabled.Swap(on)
	return func() { enabled.Store(prev) }
}

// Reset drops every registered loop (tests).
func Reset() {
	regMu.Lock()
	reg = nil
	regMu.Unlock()
}

// NewLoop returns the state of loop id, registered for [Snapshot], or nil when
// the gate is off.
func NewLoop(id int) *Loop {
	if !enabled.Load() {
		return nil
	}
	return NewLoopShift(id, SampleShift)
}

// NewLoopShift is NewLoop with another sampling rate (1 in 1<<shift) and
// without the gate check, for tests.
func NewLoopShift(id int, shift uint) *Loop {
	l := &Loop{id: id, shift: shift}
	l.rng = seed(uint64(id)*2 + 1)
	l.turnRng = seed(uint64(id)*2 + 2)
	regMu.Lock()
	reg = append(reg, l)
	regMu.Unlock()
	return l
}

// seed is splitmix64 of s: non-zero, and different per loop.
func seed(s uint64) uint64 {
	s += 0x9E3779B97F4A7C15
	s = (s ^ (s >> 30)) * 0xBF58476D1CE4E5B9
	s = (s ^ (s >> 27)) * 0x94D049BB133111EB
	s ^= s >> 31
	if s == 0 {
		s = 1
	}
	return s
}

// draw advances an xorshift64* state and reports whether the draw is one in
// 1<<shift. A counter would alias to one connection when a loop serves 2: the
// draw is random per recv (DESIGN-757 r2).
func draw(x *uint64, shift uint) bool {
	s := *x
	s ^= s >> 12
	s ^= s << 25
	s ^= s >> 27
	*x = s
	return (s*0x2545F4914F6CDD1D)>>(64-shift) == 0 // top bits; shift 0 samples all
}

// SampleRecv reports whether this recv is sampled. Loop thread only.
func (l *Loop) SampleRecv() bool { return draw(&l.rng, l.shift) }

// SampleTurn reports whether this loop turn is sampled. Loop thread only.
func (l *Loop) SampleTurn() bool { return draw(&l.turnRng, l.shift) }

// ID is the loop's number.
func (l *Loop) ID() int { return l.id }

// HistSnap is a snapshot of a histogram. N is the sum of B by construction;
// SumNS is read a moment apart from the buckets, so it can include one sample
// more than N.
type HistSnap struct {
	N     uint64
	SumNS uint64
	B     [NumBuckets]uint64
}

// Snap is the cumulative state of all loops.
type Snap struct {
	N      uint64 // sampled recvs that carried requests and were timed
	Unst   uint64 // sampled recvs that carried a pool-bound stream: not timed
	Part   uint64 // the response write was partial: not timed
	NoWr   uint64 // no write came after the sampled recv (EAGAIN, or none): not timed
	Early  uint64 // a write returned before ProcessH2 did (T_wr < T_proc): not timed
	InProc uint64 // write(2) calls on the sampled conn made while ProcessH2 was running (the guarded-writeFn question)
	Loops  []uint64
	H      [NumHists]HistSnap
}

// Snapshot sums the registered loops. Safe to call from any goroutine.
func Snapshot() Snap {
	regMu.Lock()
	ls := append([]*Loop(nil), reg...)
	regMu.Unlock()
	var s Snap
	for _, l := range ls {
		n := l.N.Load()
		s.N += n
		s.Unst += l.Unst.Load()
		s.Part += l.Part.Load()
		s.NoWr += l.NoWr.Load()
		s.Early += l.Early.Load()
		s.InProc += l.InProc.Load()
		for len(s.Loops) <= l.id {
			s.Loops = append(s.Loops, 0)
		}
		s.Loops[l.id] += n
		for i := range l.H {
			h := l.H[i].snap()
			d := &s.H[i]
			d.N += h.N
			d.SumNS += h.SumNS
			for j := range h.B {
				d.B[j] += h.B[j]
			}
		}
	}
	return s
}
