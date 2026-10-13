package singleflight

import (
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// celeris#888 item 24, review round 2. Canonicalising Accept-Encoding runs the
// negotiator's parser over the whole value, and that parser grows a slice by
// one entry per list item with no limit: "a,a,a,..." cost about 66 times its
// length in memory and about 200 times main's CPU, on every request that
// reached the middleware (the std engine accepts a header of 16 MiB by
// default, the native engines 64 KiB), and again for every waiter whose
// leader's response varies on it. A value longer than maxCanonicalAE is used
// as sent: equal values negotiate alike, so the only cost is a coalescing
// missed between spellings of one long list.

// bytesPer is the heap bytes f allocates per call, averaged over n calls.
func bytesPer(n int, f func()) float64 {
	f() // warm
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	for range n {
		f()
	}
	runtime.ReadMemStats(&b)
	return float64(b.TotalAlloc-a.TotalAlloc) / float64(n)
}

// allocsPer is the fewest allocations per call of f that any of up to
// allocWindows single-call windows sees, and it stops at the first window that
// is at most limit. testing.AllocsPerRun counts the whole process's mallocs, not
// the calling goroutine's, and this process has a goroutine that allocates once
// a second whatever the test does: internal/conn refreshes its cached Date
// header every second, 14 mallocs per tick (seven buffers, each stored through
// a pointer that is itself allocated). AllocsPerRun divides by the number of
// runs and drops the remainder, so one tick inside the 16 MiB subtest's three
// runs reads (3+14)/3 = 5.0 allocations against a bound of 2 (celeris#890:
// the Coverage job failed three times so); the 64 KiB subtest's eight runs
// read (8+14)/8 = 2 and pass by that rounding alone. A tick lands in at most
// one single-call window, since a call takes far less than a second, so the
// least of several windows is the cost of the call alone; a call that
// allocates more than the bound allocates it in every window, and the least
// is still over it.
func allocsPer(limit float64, f func()) float64 {
	best := math.MaxFloat64
	for range allocWindows {
		best = min(best, testing.AllocsPerRun(1, f))
		if best <= limit {
			break
		}
	}
	return best
}

// allocWindows is how many single-call windows allocsPer tries at most.
const allocWindows = 8

// worstAE is the dearest value of a given size for the parser: one entry per
// two bytes.
func worstAE(size int) string { return strings.Repeat("a,", size/2) }

// TestLongAcceptEncodingCostsAboutItsLength888 covers the three places that
// read the value: the default key, captureVary (the leader's copy) and
// sameVariant (each waiter's comparison). Each may copy the value once (the
// key holds it, and the leader's copy must outlive its connection buffer),
// never more, and a comparison copies nothing.
func TestLongAcceptEncodingCostsAboutItsLength888(t *testing.T) {
	for _, size := range []int{64 << 10, 16 << 20} {
		v := worstAE(size)
		name := "64KiB"
		if size > 1<<20 {
			name = "16MiB"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := celeristest.NewContext("GET", "/doc", celeristest.WithHeader("accept-encoding", v))
			defer celeristest.ReleaseContext(c)
			n := 8
			if size > 1<<20 {
				n = 3
			}
			// One copy, and the allocator's rounding up to its size class or
			// page, and a little for the key's other components.
			slack := float64(size/8 + 4<<10)

			var key string
			if got := bytesPer(n, func() { key = defaultKeyFunc(c) }); got > float64(size)+slack {
				t.Errorf("defaultKeyFunc allocated %.0f bytes for a %d-byte Accept-Encoding, want at most one copy (%d) and a little", got, size, size)
			}
			if allocs := allocsPer(2, func() { _ = defaultKeyFunc(c) }); allocs > 2 {
				t.Errorf("defaultKeyFunc made %.1f allocations for a %d-byte Accept-Encoding, want at most 2", allocs, size)
			}
			if !strings.Contains(key, v) {
				t.Errorf("the key does not hold the value as sent")
			}

			e := &call{headers: [][2]string{{"vary", "Accept-Encoding"}}}
			if got := bytesPer(n, func() { e.vary = e.vary[:0]; e.captureVary(c) }); got > float64(size)+slack {
				t.Errorf("captureVary allocated %.0f bytes for a %d-byte Accept-Encoding, want at most one copy (%d) and a little", got, size, size)
			}
			if allocs := allocsPer(3, func() { e.vary = e.vary[:0]; e.captureVary(c) }); allocs > 3 {
				t.Errorf("captureVary made %.1f allocations for a %d-byte Accept-Encoding, want at most 3", allocs, size)
			}

			same := true
			if got := bytesPer(n, func() { same = e.sameVariant(c) }); got > 4<<10 {
				t.Errorf("sameVariant allocated %.0f bytes for a %d-byte Accept-Encoding, want none", got, size)
			}
			if !same {
				t.Errorf("sameVariant: a request sending the leader's own value is not the same variant")
			}
			// A waiter that sends another long value is another variant.
			other, _ := celeristest.NewContext("GET", "/doc", celeristest.WithHeader("accept-encoding", v+"b"))
			defer celeristest.ReleaseContext(other)
			if e.sameVariant(other) {
				t.Errorf("sameVariant: a different long value is the same variant")
			}
		})
	}
}
