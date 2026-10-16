package otel

import (
	"math"
	"strconv"
	"testing"
	"unsafe"

	"github.com/goceleris/celeris/celeristest"
)

// allocsPer888 is the least of several single-call windows of
// testing.AllocsPerRun: the process-wide counter also sees the goroutine
// internal/conn runs (14 mallocs once per second), which lands in a window now
// and then; a call that allocates more than limit allocates it in every
// window.
func allocsPer888(limit float64, f func()) float64 {
	best := math.MaxFloat64
	for range 8 {
		best = min(best, testing.AllocsPerRun(1, f))
		if best <= limit {
			break
		}
	}
	return best
}

const tp888 = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// celeris#888 item 1: Get("traceparent") is the one value no propagator keeps
// (W3C TraceContext decodes it into the fixed-size trace and span IDs), so
// it is not copied; every other value is, because ParseTraceState and
// baggage.Parse return substrings of their input and the span context that
// holds them outlives the request. The control is the aliasing check on the
// copied ones: the string Get returns must not share memory with the request's.
func TestCarrierGetCopiesAllButTraceparent888(t *testing.T) {
	c, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("traceparent", tp888),
		celeristest.WithHeader("tracestate", "vendor=value-of-some-length"),
		celeristest.WithHeader("baggage", "k=v-of-some-length"),
		celeristest.WithHeader("x-other", "other-value"),
	)
	defer celeristest.ReleaseContext(c)
	h := headerCarrier{ctx: c}

	if got := h.Get("traceparent"); got != tp888 {
		t.Fatalf("Get(traceparent) = %q, want %q", got, tp888)
	}
	if n := allocsPer888(0, func() { _ = h.Get("traceparent") }); n != 0 {
		t.Errorf("Get(traceparent) allocates %.0f, want 0", n)
	}
	for _, key := range []string{"tracestate", "baggage", "x-other"} {
		want := c.Header(key)
		got := h.Get(key)
		if got != want || got == "" {
			t.Fatalf("Get(%s) = %q, want %q", key, got, want)
		}
		if unsafe.StringData(got) == unsafe.StringData(want) {
			t.Errorf("Get(%s) shares memory with the request's header: a substring a propagator keeps would alias the receive buffer", key)
		}
	}
	if got := h.Get("absent"); got != "" {
		t.Errorf("Get(absent) = %q, want empty", got)
	}
	if n := allocsPer888(0, func() { _ = h.Get("absent") }); n != 0 {
		t.Errorf("Get(absent) allocates %.0f, want 0", n)
	}
}

// celeris#888 item 1: Keys de-duplicates, skips pseudo-headers, returns copies
// (not views of the request) and builds the names from one allocation, with
// the slice itself the second.
func TestCarrierKeysOneAllocationForTheNames888(t *testing.T) {
	for _, n := range []int{3, 12, 30, 31, 40, 100} { // below and above the linear-dedupe threshold
		opts := []celeristest.Option{
			celeristest.WithHeader("traceparent", tp888),
			celeristest.WithHeader("x-dup", "1"),
			celeristest.WithHeader("x-dup", "2"),
		}
		want := map[string]bool{"traceparent": true, "x-dup": true}
		for i := range n {
			k := "x-header-" + strconv.Itoa(i)
			opts = append(opts, celeristest.WithHeader(k, "v"))
			want[k] = true
		}
		// A late duplicate: it meets the names the linear scan or the map keeps.
		opts = append(opts, celeristest.WithHeader("x-header-0", "again"))
		c, _ := celeristest.NewContext("GET", "/", opts...)
		h := headerCarrier{ctx: c}
		keys := h.Keys()
		if len(keys) != len(want) {
			t.Errorf("n=%d: Keys returned %d names, want %d: %q", n, len(keys), len(want), keys)
		}
		views := map[*byte]bool{}
		for _, hdr := range c.RequestHeaders() {
			views[unsafe.StringData(hdr[0])] = true
		}
		for _, k := range keys {
			if !want[k] {
				t.Errorf("n=%d: unexpected or duplicate name %q", n, k)
			}
			delete(want, k)
			if k != "" && views[unsafe.StringData(k)] {
				t.Errorf("n=%d: name %q is a view of the request, not a copy", n, k)
				break
			}
		}
		// The test context's RequestHeaders may copy (an HTTP/2 stream does);
		// what Keys adds to that is the bound.
		base := allocsPer888(0, func() { _ = c.RequestHeaders() })
		if n+2 <= 32 { // 32 is keysLinearDedupe: above it the de-duplicating map is one more
			if a := allocsPer888(base+2, func() { _ = h.Keys() }); a > base+2 {
				t.Errorf("n=%d: Keys allocates %.0f (RequestHeaders alone %.0f), want at most 2 more (the slice and one buffer for every name)", n, a, base)
			}
		}
		celeristest.ReleaseContext(c)
	}
}
