package celeris

import (
	"context"
	"testing"
	"time"
)

// celeris#886 (from #852's round-2 review): every request no route matches
// shares one adaptive key (unmatchedAdaptiveKey). Fast unmatched traffic
// (scanner 404s, a healthcheck mounted with Use) used to settle it after
// adaptiveSettleStreak runs; a later unmatched request whose chain blocks
// (pprof, an auth middleware) then ran inline on the engine worker, untimed
// and unpromoted, until the settle re-opener happened to time it. The key
// never settles, so it stays timed and a blocking run promotes it.

// newUnmatchedAdapter886 builds an adapter the way Start does, under the
// AsyncHandlers default with one global middleware that sleeps for the
// duration the request's path names: "/slow" 500us (over
// adaptivePromoteThreshold, under adaptiveBlockingThreshold), "/block" 5 ms
// (over adaptiveBlockingThreshold), anything else not at all.
func newUnmatchedAdapter886(t *testing.T) (*routerAdapter, *router) {
	t.Helper()
	s := New(Config{AsyncHandlers: true})
	s.Use(func(c *Context) error {
		switch c.Path() {
		case "/slow":
			time.Sleep(500 * time.Microsecond)
		case "/block":
			time.Sleep(5 * time.Millisecond)
		}
		return c.Next()
	})
	s.GET("/ping", func(c *Context) error { return c.String(200, "pong") })
	ra := &routerAdapter{server: s}
	ra.buildUnmatchedChains()
	if !s.router.unmatchedAdaptive {
		t.Fatal("the unmatched chain is not adaptive under AsyncHandlers with a global middleware")
	}
	return ra, s.router
}

func serve886(t *testing.T, ra *routerAdapter, method, path string) {
	t.Helper()
	st, _ := newTestStream(method, path)
	if err := ra.HandleStream(context.Background(), st); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	st.Release()
}

// TestUnmatchedKeyNeverSettles886: after more fast unmatched runs than
// adaptiveSettleStreak the key is still timed, and the first run that blocks
// promotes it, so routeAsync answers true for every unmatched request.
func TestUnmatchedKeyNeverSettles886(t *testing.T) {
	ra, rt := newUnmatchedAdapter886(t)
	for range 4 * adaptiveSettleStreak {
		serve886(t, ra, "GET", "/nope")
	}
	if _, settled := rt.settled.Load(unmatchedAdaptiveKey); settled {
		t.Fatalf("the unmatched key settled after %d fast runs: a blocking unmatched request would run inline untimed", 4*adaptiveSettleStreak)
	}
	if !rt.adaptiveLearning(unmatchedAdaptiveKey) && !rt.isPromoted(unmatchedAdaptiveKey) {
		t.Fatal("the unmatched key is neither learning nor promoted")
	}
	if rt.isPromoted(unmatchedAdaptiveKey) {
		t.Skip("a fast run took over adaptiveBlockingThreshold on this runner and promoted the key; the settle path was exercised")
	}
	serve886(t, ra, "GET", "/block")
	for _, rq := range [][2]string{{"GET", "/nope"}, {"HEAD", "/nope"}, {"DELETE", "/ping"}, {"OPTIONS", "/ping"}} {
		if !rt.routeAsync(rq[0], rq[1]) {
			t.Errorf("after one blocking run on a key that saw %d fast ones: routeAsync(%s %s) = false, want true", 4*adaptiveSettleStreak, rq[0], rq[1])
		}
	}
	if rt.routeAsync("GET", "/ping") {
		t.Error("routeAsync(GET /ping) = true: the matched route was promoted by the unmatched key")
	}
}

// TestUnmatchedKeySlowStreakPromotes886: the 300 us - 2 ms band, which no
// single run promotes, promotes the unmatched key after adaptivePromoteStreak
// consecutive slow runs, including after a long fast prefix. Pins the slow
// path of recordInlineRun for the unmatched key, which no test of #852 did: a
// mutant that dropped the call passed them all.
func TestUnmatchedKeySlowStreakPromotes886(t *testing.T) {
	ra, rt := newUnmatchedAdapter886(t)
	for range 2 * adaptiveSettleStreak {
		serve886(t, ra, "GET", "/nope")
	}
	if rt.routeAsync("GET", "/nope") {
		t.Skip("a fast run took over adaptiveBlockingThreshold on this runner and promoted the key")
	}
	for range adaptivePromoteStreak {
		if rt.routeAsync("GET", "/nope") {
			break // a slow run took over adaptiveBlockingThreshold: promoted early, as designed
		}
		serve886(t, ra, "GET", "/slow")
	}
	if !rt.routeAsync("GET", "/nope") {
		t.Errorf("after %d consecutive slow (500us) unmatched runs routeAsync(GET /nope) = false, want true", adaptivePromoteStreak)
	}
}
