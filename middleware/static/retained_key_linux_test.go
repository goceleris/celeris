//go:build linux

package static

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

func engineKinds(t *testing.T) []celeris.EngineType {
	t.Helper()
	kinds := []celeris.EngineType{celeris.Epoll}
	p := probe.Probe()
	if p.IOUringTier >= celerisengine.High && p.ProvidedBuffers {
		kinds = append(kinds, celeris.IOUring)
	} else {
		t.Logf("io_uring tier=%s kernel=%s — skipping IOUring sub-test", p.IOUringTier.String(), p.KernelVersion)
	}
	return kinds
}

func waitForReady(tb testing.TB, s *celeris.Server, timeout time.Duration) string {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if addr := s.Addr(); addr != nil {
			a := addr.String()
			if conn, err := net.DialTimeout("tcp", a, 100*time.Millisecond); err == nil {
				_ = conn.Close()
				return a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	tb.Fatal("server not ready within timeout")
	return ""
}

// TestFSCacheKeyIsNotAliasedToRequestBuffer is the regression guard for the
// static file cache retaining a sub-slice of c.Path() as its sync.Map key.
// On the native engines c.Path() is a zero-copy view over the connection's
// read buffer, so the retained key's bytes changed under the map on the
// next request and the runtime's hash trie eventually failed:
//
//	panic: internal/sync.HashTrieMap: ran out of hash bits while inserting
//	(incorrect use of unsafe or cgo, or data race?)
//
// recovered into a 500 -- 67,301 times in a 150 s flood on io_uring,
// 104,302 on epoll, 0 on std (which copies the path). A plain keep-alive
// flood of one cached file reproduces it in seconds.
func TestFSCacheKeyIsNotAliasedToRequestBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("native-engine flood")
	}
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><body>static</body></html>")},
		"a.txt":      &fstest.MapFile{Data: []byte("aaaa")},
		"b.txt":      &fstest.MapFile{Data: []byte("bbbb")},
	}
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			s := celeris.New(celeris.Config{Engine: kind})
			s.Use(New(Config{FS: fsys, Prefix: "/static"}))
			// placeholder route so the router does not 404 before the
			// middleware intercepts (mirrors the probatorium refapp)
			s.GET("/static/*filepath", func(c *celeris.Context) error { return c.String(404, "not intercepted") })
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
			addr := waitForReady(t, s, 30*time.Second)
			defer func() { cancel(); <-done }()

			// Alternate paths on each keep-alive connection so the read
			// buffer that backed a cached key is overwritten by a different
			// path on the very next request.
			paths := []string{"/static/index.html", "/static/a.txt", "/static/b.txt"}
			var non200, sent atomic.Int64
			var wg sync.WaitGroup
			stop := time.Now().Add(3 * time.Second)
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					tr := &http.Transport{MaxIdleConnsPerHost: 1}
					hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
					for n := 0; time.Now().Before(stop); n++ {
						resp, err := hc.Get("http://" + addr + paths[(i+n)%len(paths)])
						if err != nil {
							non200.Add(1)
							continue
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						sent.Add(1)
						if resp.StatusCode != 200 {
							non200.Add(1)
						}
					}
				}(i)
			}
			wg.Wait()
			t.Logf("%s: sent=%d non200=%d", kind, sent.Load(), non200.Load())
			if non200.Load() != 0 {
				t.Errorf("%d of %d static responses were not 200 on %s: the fs cache retained a key aliased to the request buffer", non200.Load(), sent.Load(), kind)
			}
		})
	}
}
