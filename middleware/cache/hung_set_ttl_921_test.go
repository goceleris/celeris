package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/store"
)

// celeris#921 (review of the fix): while the leader's store Set runs, the key
// stays held with the leader's published response, so a request that comes
// meanwhile takes it without running the handler. A Set that never returns
// (a remote store without a timeout) must not make that response outlive its
// TTL: a request that comes after the TTL has passed runs its own handler, as
// it would once the stored entry expired.
func TestCacheHungSetResultExpiresWithItsTTL921(t *testing.T) {
	// The past-TTL arms only need the TTL to have passed, which a sleep
	// guarantees whatever the scheduling, so their TTL is short. The
	// within-TTL arm needs the second request served before the TTL ends,
	// which a GC pause or a loaded -race runner can delay: its TTL is long.
	const ttl, longTTL = 50 * time.Millisecond, time.Minute
	for _, arm := range []struct {
		name    string
		hangSet bool
		ttl     time.Duration
		wait    time.Duration
		want    string
		wantHit string
	}{
		// The defect: the leader's Set hangs, the TTL is long past.
		{"set-hangs-past-ttl", true, ttl, 6 * ttl, "v2", "MISS"},
		// Within the TTL the held response is still handed out (a HIT: the
		// handler does not run again).
		{"set-hangs-within-ttl", true, longTTL, 0, "v1", "HIT"},
		// Control: the Set returns; the stored entry expires on its own.
		{"set-returns-past-ttl", false, ttl, 6 * ttl, "v2", "MISS"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			inSet, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			kv := &hookStore{KV: store.NewMemoryKV(), set: func(n int32) {
				if n == 1 {
					close(inSet)
					if arm.hangSet {
						<-release
					}
				}
			}}
			var version atomic.Int32
			version.Store(1)
			var runs atomic.Int32
			h := func(c *celeris.Context) error {
				runs.Add(1)
				if version.Load() == 1 {
					return c.String(200, "v1")
				}
				return c.String(200, "v2")
			}
			mw := New(Config{Store: kv, TTL: arm.ttl})

			first := serve921(t, mw, h)
			select {
			case <-inSet:
			case <-time.After(5 * time.Second):
				t.Fatal("the leader's Set never started")
			}
			if !arm.hangSet {
				if a := await921(t, "first", first, 3*time.Second); a.body != "v1" {
					t.Fatalf("first: %q, want v1", a.body)
				}
			}
			time.Sleep(arm.wait)
			if arm.wait > 0 {
				version.Store(2) // the data changed after the TTL passed
			}
			second := await921(t, "second", serve921(t, mw, h), 3*time.Second)
			if second.body != arm.want || (arm.wantHit != "" && second.xcache != arm.wantHit) {
				t.Errorf("a request %v after the fill (TTL %v): body %q x-cache %q, want %q %s", arm.wait, arm.ttl, second.body, second.xcache, arm.want, arm.wantHit)
			}
			t.Logf("second: %d %q x-cache %q; handler runs %d", second.status, second.body, second.xcache, runs.Load())
			once.Do(func() { close(release) })
			if arm.hangSet {
				if a := await921(t, "first (leader)", first, 3*time.Second); a.body != "v1" || a.xcache != "MISS" {
					t.Errorf("leader: %q x-cache %q, want v1 MISS", a.body, a.xcache)
				}
			}
		})
	}
}
