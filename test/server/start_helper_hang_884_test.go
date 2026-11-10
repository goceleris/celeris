package celeris_test

import (
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#884: startServerConfig852 (the start helper of the #852 tests, and of
// every test that uses startServer852) waited for Start's result twice when
// Start returned an error before the server was ready: waitReady852 took the
// error from the channel, and the helper then waited on the same buffered
// channel, which holds one value, for good. A start that fails early, which
// is what an io_uring ring's ENOMEM under another process's locked memory is,
// hung the test until the package's timeout instead of failing it or being
// retried (the hang seen twice on a loaded Docker VM, TestUseMountedMiddlewares
// AnswerTheirPathsNativeEngines852/io_uring/plain).
func TestStartHelperReturnsAnEarlyStartFailure884(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // Start fails at its first Accept
	type result struct {
		addr string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		addr, _, err := startOnce852(celeris.Config{Engine: celeris.Std}, func(*celeris.Server) {}, ln)
		res <- result{addr, err}
	}()
	select {
	case r := <-res:
		if r.err == nil {
			t.Fatalf("a server on a closed listener started, at %s", r.addr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start helper did not return within 10s of a Start that failed at once: it waits twice for the one result")
	}
}
