//go:build linux

package websocket

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

// This file measures the celeris#672 wedge rate DIRECTLY, per connection. It
// is the harness behind the PR #671 A/B that showed the celeris#667 fix alone
// makes the stale-pause wedge more likely at the production default, and that
// the celeris#672 re-check in requestPause takes it to zero.
//
// Why the benchmark's forced/op could not answer that. BenchmarkChanReader-
// Contended carries an escape hatch: when it finds the engine paused with an
// empty buffer it delivers a chunk anyway and counts that as forced/op. That
// number conflates two different things. On the unfixed arm a wedge is usually
// PERMANENT, so one wedge drives forced/op to 0.26-1.0 and the sample
// degenerates; on the #667-only arm each wedge is rescued by the hatch and the
// run continues, so many distinct wedges accumulate at ~1e-4. A ratio between
// those two is not a rate ratio, it is two different failure shapes divided by
// each other.
//
// What this harness does instead: it REMOVES the escape hatch, runs each
// connection until it wedges or exhausts its op budget, and reports
// ops-to-first-wedge and edges-to-first-wedge per connection. One wedge ends
// one connection, which is exactly the production consequence — a wedged
// connection never delivers again, and production has no hatch.
//
// The wedge is detected STRUCTURALLY, not by a timeout: the engine is paused,
// the buffer is empty, and the reader has made no progress across a bounded
// number of scheduler yields. On a chanReader without the celeris#672 fix that
// state cannot resolve on its own, because Read re-evaluates the resume only
// after a successful dequeue and the dequeue can never happen while the engine
// is paused with an empty buffer. A wall-clock deadline would make the
// measurement a function of machine load; the yield-bounded progress check is
// load-independent.
//
// NEGATIVE CONTROL. The tree this file ships in carries the celeris#672 fix,
// so run as-is it must report zero wedges at matched edge rates; a detector
// that never reports zero is measuring itself. The positive arms are the same
// file run against the unfixed engineread.go, taken from git:
//
//	# base: before celeris#667 and #672 (any main before PR #671)
//	git show 9f4d89b:middleware/websocket/engineread.go > middleware/websocket/engineread.go
//	# #667 only: this tree's engineread.go with the celeris#672 re-check
//	# block at the end of requestPause deleted
//
// then, on Linux (the stub uses a real eventfd), one process per observation:
//
//	WS667_WEDGE=1 WS667_WEDGE_CAP=256 WS667_WEDGE_CONNS=16 WS667_WEDGE_OPS=1000000 \
//	  go test -count=1 -v -run '^TestMeasureWedgeRate$' ./middleware/websocket/
//
// Restore the file with `git checkout middleware/websocket/engineread.go`. The
// test is deliberately named outside the ^TestChanReader namespace so the CI
// step that runs the chanReader tests by regex never sweeps it in; without
// WS667_WEDGE it skips.

var errWedgeUnwind = errors.New("wedge harness: unwinding a wedged connection")

// wedgeResult is one connection's outcome.
type wedgeResult struct {
	wedged  bool
	ops     int // ops completed when the wedge was declared, else the budget
	edges   int64
	pauses  int64
	resumes int64
	wakeups int64
	probes  int64 // candidate-wedge states inspected and found transient
}

// runWedgeConn drives ONE chanReader to its first celeris#672 wedge or to the
// op budget. The producer models an engine worker: it delivers nothing while
// recv is paused, and it drains the detach queue as it goes.
func runWedgeConn(t *testing.T, capacity, maxOps, spin, maxProbes int) wedgeResult {
	t.Helper()

	stub := newEnginePauseStub(t)
	r := newChanReader(capacity, 0, 0)
	r.SetPauser(stub.pause, stub.resume)

	var reads atomic.Int64
	var consumerDone atomic.Bool

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer consumerDone.Store(true)
		buf := make([]byte, 1)
		for i := 0; i < maxOps; i++ {
			if _, err := r.Read(buf); err != nil {
				return
			}
			reads.Add(1)
		}
	}()

	chunk := []byte{'x'}
	res := wedgeResult{ops: maxOps}

	for i := 0; i < maxOps; i++ {
		wedged := false
		// A paused connection receives nothing. There is deliberately NO
		// escape hatch here: the only ways out of this loop are the engine
		// being resumed, or the wedge being declared.
		for stub.desired.Load() {
			stub.drain()
			if len(r.ch) != 0 {
				continue
			}
			if consumerDone.Load() {
				// The consumer finished its budget; a paused-and-empty state
				// now is the end of the run, not a wedge.
				break
			}
			// Candidate wedge. Confirm the state persists while the reader
			// makes no progress.
			before := reads.Load()
			stuck := true
			for k := 0; k < spin; k++ {
				runtime.Gosched()
				if !stub.desired.Load() || len(r.ch) != 0 ||
					reads.Load() != before || consumerDone.Load() {
					stuck = false
					break
				}
			}
			if stuck {
				wedged = true
				break
			}
			res.probes++
			if res.probes > int64(maxProbes) {
				// Not progressing and not resolving either. Counting this as
				// a wedge is the conservative choice: it cannot inflate a
				// difference between arms unless one arm actually spends far
				// longer in the paused-and-empty state, which is the very
				// thing being measured.
				wedged = true
				break
			}
		}
		if wedged {
			res.wedged = true
			res.ops = i
			break
		}
		if !r.Append(chunk) {
			t.Fatalf("append %d rejected (ErrReadLimit) — the workload outran the spill", i)
		}
		stub.drain()
	}

	// Unwind without any timing dependence: closeWith closes `done`, which
	// wakes a Read parked in its blocking select, so the consumer returns.
	r.closeWith(errWedgeUnwind)
	wg.Wait()

	res.edges = stub.edges.Load()
	res.pauses = stub.pauseCalls.Load()
	res.resumes = stub.resumeCalls.Load()
	res.wakeups = stub.wakeups.Load()
	return res
}

// TestMeasureWedgeRate is a MEASUREMENT harness, not a pass/fail test: it
// prints one WEDGE_CONN line per connection and one WEDGE_SUMMARY line, which
// the A/B analysis parses. It is gated on WS667_WEDGE so it never runs in a
// normal test pass, and it asserts its own anti-vacuity conditions so it
// cannot report a clean zero because nothing happened.
func TestMeasureWedgeRate(t *testing.T) {
	if os.Getenv("WS667_WEDGE") == "" {
		t.Skip("measurement harness; set WS667_WEDGE=1 to run")
	}
	capacity := envInt("WS667_WEDGE_CAP", 16)
	conns := envInt("WS667_WEDGE_CONNS", 32)
	maxOps := envInt("WS667_WEDGE_OPS", 200000)
	spin := envInt("WS667_WEDGE_SPIN", 512)
	maxProbes := envInt("WS667_WEDGE_MAXPROBES", 4096)

	results := make([]wedgeResult, 0, conns)
	for i := 0; i < conns; i++ {
		res := runWedgeConn(t, capacity, maxOps, spin, maxProbes)
		results = append(results, res)
		fmt.Printf("WEDGE_CONN idx=%d cap=%d wedged=%t ops=%d edges=%d pauses=%d resumes=%d wakeups=%d probes=%d\n",
			i, capacity, res.wedged, res.ops, res.edges, res.pauses, res.resumes, res.wakeups, res.probes)
	}

	// Anti-vacuity: every connection must actually have exercised the
	// watermark path, or "no wedges" would be meaningless.
	var totalEdges, totalOps int64
	nWedged := 0
	opsToWedge := make([]float64, 0, conns)
	edgesToWedge := make([]float64, 0, conns)
	for i, res := range results {
		if res.edges == 0 || res.pauses == 0 {
			t.Fatalf("conn %d never crossed a watermark (edges=%d pauses=%d): "+
				"the harness is vacuous at cap=%d", i, res.edges, res.pauses, capacity)
		}
		totalEdges += res.edges
		totalOps += int64(res.ops)
		if res.wedged {
			nWedged++
			opsToWedge = append(opsToWedge, float64(res.ops))
			edgesToWedge = append(edgesToWedge, float64(res.edges))
		}
	}

	med := func(xs []float64) float64 {
		if len(xs) == 0 {
			return -1
		}
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		if len(s)%2 == 1 {
			return s[len(s)/2]
		}
		return (s[len(s)/2-1] + s[len(s)/2]) / 2
	}

	// Per-EDGE is the like-for-like rate: a wedge can only be created by a
	// pause decision, so if the two arms cross the watermarks at different
	// rates, per-op would compare different numbers of opportunities.
	wedgesPerMegaOp := float64(nWedged) / float64(max64(totalOps, 1)) * 1e6
	wedgesPerKiloEdge := float64(nWedged) / float64(max64(totalEdges, 1)) * 1e3

	fmt.Printf("WEDGE_SUMMARY cap=%d conns=%d maxOps=%d wedged=%d frac=%.4f "+
		"medOpsToWedge=%.1f medEdgesToWedge=%.1f edgesPerOp=%.6f "+
		"wedgesPerMegaOp=%.3f wedgesPerKiloEdge=%.4f\n",
		capacity, conns, maxOps, nWedged, float64(nWedged)/float64(conns),
		med(opsToWedge), med(edgesToWedge),
		float64(totalEdges)/float64(max64(totalOps, 1)),
		wedgesPerMegaOp, wedgesPerKiloEdge)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
