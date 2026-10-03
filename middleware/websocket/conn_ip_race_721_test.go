package websocket

import (
	"context"
	"net"
	"sync"
	"testing"
)

// TestConnIPConcurrentCalls pins that Conn.IP only reads: the read and the
// write goroutine of a Conn may both call it, for example to log. It used to
// cache the IP on its first call, an unsynchronized write that -race reports
// when two goroutines make that first call. celeris#721 gives the engine
// path an IP too, so the IP is now computed once, at upgrade (setupConn).
// This runs the hijack path's Conn (the std engine's), which had the cache
// before celeris#721.
func TestConnIPConcurrentCalls(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := newConn(ctx, cancel, a, 0, 0)
	setupConn(ws, &Config{}, false, nil, nil)

	got := make([]string, 2)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = ws.IP()
		}()
	}
	wg.Wait()
	t.Logf("MW721IP got=%q", got)
	if want := a.RemoteAddr().String(); got[0] != want || got[1] != want {
		t.Fatalf("IP() = %q, want %q from both goroutines", got, want)
	}
}
