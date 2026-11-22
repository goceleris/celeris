//go:build linux && !race

package celeris_test

import (
	"testing"
	"time"
)

// The bounds of TestH2XNetLargeResponseVsStd809 on each native engine: its
// elapsed time is at most h2XNetNativeFactor809 times std's, measured in the
// same process with the same client, but never held below h2XNetNativeFloor809
// (a std sample of 0.06 s would otherwise give a 0.30 s bar that a shared
// runner's noise can trip) and never allowed above h2XNetNativeCap809 in
// absolute terms. The defect was 16 s against std's 0.07 s: the floor still
// catches it by a factor of 32, the factor and the cap catch a slow runner
// that slows std too.
const (
	h2XNetNativeFactor809 = 5
	h2XNetNativeFloor809  = 500 * time.Millisecond
	h2XNetNativeCap809    = 2 * time.Second
)

// TestH2XNetLargeResponseVsStd809 is the regression test of celeris#809 (a
// 64 MiB HTTP/2 response to golang.org/x/net's http2.Transport took about 16 s
// on epoll, io_uring and adaptive and 0.07 s on std; fixed by #965). For each
// route it runs the 64 MiB transfer of probe761H2Cell, the one TestProbe761H2
// measures, on std first and then on each native engine, and requires of each
// native engine: the transfer ends complete (every byte of the body received,
// which probe761H2Cell itself checks), the engine that ran is the one asked
// for (an unavailable io_uring fails here, it does not pass without a
// transfer: the CI job that runs this test has it), and its elapsed time is at
// most h2XNetNativeFactor809 times std's of the same route, with that limit
// held between h2XNetNativeFloor809 and h2XNetNativeCap809. Every failure names
// the measured times.
//
// A plain `go test ./test/server` on a Linux host without io_uring or enough
// memlock (WSL, containers) fails this test by design: the engine that ran
// must be the one asked for.
//
// The test is for a plain build: the time of a transfer under -race or
// coverage says nothing about the engine (the race detector alone costs it
// 1.5-2x), so it is not built under -race and skips under coverage. The CI job
// `h2-xnet-large` runs it on both architectures.
func TestH2XNetLargeResponseVsStd809(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("timings under coverage say nothing about the engine")
	}
	const n = 64 << 20
	for _, route := range []string{"sync", "async"} {
		t.Run(route, func(t *testing.T) {
			async := route == "async"
			base := probe761H2Cell(t, "std", engines761[0].eng, async, n)
			if !base.ran || base.running != "std" || base.end != "complete" {
				t.Fatalf("the std baseline did not complete: ran=%v running=%s end=%s elapsed=%.3fs", base.ran, base.running, base.end, base.elapsed.Seconds())
			}
			t.Logf("H2XNET809 route=%s engine=std elapsed=%.3fs", route, base.elapsed.Seconds())
			limit := min(max(h2XNetNativeFactor809*base.elapsed, h2XNetNativeFloor809), h2XNetNativeCap809)
			for _, e := range engines761[1:] {
				t.Run(e.name, func(t *testing.T) {
					r := probe761H2Cell(t, e.name, e.eng, async, n)
					t.Logf("H2XNET809 route=%s engine=%s elapsed=%.3fs std=%.3fs ratio=%.2f limit=%.3fs end=%s running=%s",
						route, e.name, r.elapsed.Seconds(), base.elapsed.Seconds(), r.elapsed.Seconds()/base.elapsed.Seconds(), limit.Seconds(), r.end, r.running)
					if !r.ran {
						t.Fatalf("%s did not run: %s", e.name, r.running)
					}
					if r.running != e.eng.String() {
						t.Fatalf("asked for %s, %s ran", e.eng, r.running)
					}
					if r.end != "complete" {
						t.Fatalf("%s: the 64 MiB transfer ended %q after %.3fs, want complete (std: %.3fs)", e.name, r.end, r.elapsed.Seconds(), base.elapsed.Seconds())
					}
					if r.elapsed > limit {
						t.Errorf("%s: 64 MiB to x/net's http2.Transport took %.3fs, over %.3fs (%dx std's %.3fs, held between %s and %s)",
							e.name, r.elapsed.Seconds(), limit.Seconds(), h2XNetNativeFactor809, base.elapsed.Seconds(), h2XNetNativeFloor809, h2XNetNativeCap809)
					}
				})
			}
		})
	}
}
