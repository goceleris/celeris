package overload

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/observe"
)

// celeris#921's family: the in-flight count was given back only when the
// handler returned, so each handler panic leaked one. With DepthThresholds
// set, enough panics left the count at the Reject threshold with nothing
// running, and every later request was rejected for good.
func TestOverloadPanicReleasesInFlight921(t *testing.T) {
	mw, ctrl := NewWithController(Config{
		CollectorProvider: func() *observe.Collector { return nil },
		DepthThresholds:   DepthThresholds{Reject: 3},
	})
	defer ctrl.Stop()
	run := func(h celeris.HandlerFunc) (status int, panicked any) {
		c, rec := celeristest.NewContext("GET", "/x", celeristest.WithHandlers(mw, h))
		defer celeristest.ReleaseContext(c)
		func() {
			defer func() { panicked = recover() }()
			_ = c.Next()
		}()
		return rec.StatusCode, panicked
	}
	for i := range 3 {
		if _, p := run(func(*celeris.Context) error { panic("boom") }); p != "boom" {
			t.Fatalf("panic %d: recovered %v, want the handler's own panic", i, p)
		}
	}
	if n := ctrl.InFlight(); n != 0 {
		t.Errorf("in-flight is %d after 3 handler panics with nothing running, want 0", n)
	}
	ok := 0
	for range 5 {
		if st, _ := run(func(c *celeris.Context) error { return c.String(200, "ok") }); st == 200 {
			ok++
		}
	}
	if ok != 5 {
		t.Errorf("after 3 handler panics %d of 5 healthy requests answered 200, want 5", ok)
	}
	t.Logf("in-flight %d; healthy requests answered 200: %d of 5", ctrl.InFlight(), ok)
}
