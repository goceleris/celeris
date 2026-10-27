//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1104: two parallel cells of TestStalledSSEEndsWithShutdown855 picked
// the same port 0.5 ms apart (the CI log shows two "celeris starting
// addr=127.0.0.1:38797" lines): one bound it, the other's Start failed with
// "address already in use", and that error surfaced at the end of the cell
// because the loser's readiness probe had been answered by the WINNER's /ping.
// A starter that picks a port, closes it and has Start bind it cannot see that;
// it has to keep the port from the pick to the bind. Here two starters are made
// to pick the very same port at the same moment, again and again, and each must
// end up with a server that answers as itself, on a port of its own.
func TestTwoStartsRacingForOnePortEndOnPortsOfTheirOwn1104(t *testing.T) {
	pick, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pick.Addr().String()
	_ = pick.Close()
	defer func(old func() (net.Listener, error)) { listenForStart1104 = old }(listenForStart1104)
	// The first picker is given the port; a picker that finds it taken (held by
	// a server) gets a fresh one, as another test's pick would.
	listenForStart1104 = func() (net.Listener, error) {
		if ln, err := net.Listen("tcp", port); err == nil {
			return ln, nil
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	deadline := time.Now().Add(4 * time.Second)
	for round := 0; round < 40 && time.Now().Before(deadline); round++ {
		var wg sync.WaitGroup
		addrs := make([]string, 2)
		answers := make([]string, 2)
		srvs := make([]*celeris.Server, 2)
		for i := range srvs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				token := fmt.Sprintf("server-%d-%d", round, i)
				s, addr, _ := startPicked1104(t, celeris.Config{Engine: celeris.Std}, func(s *celeris.Server) {
					s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", token) })
				}, func(_ *celeris.Server, addr string, done <-chan error) error { return waitReady761(addr, done) })
				srvs[i], addrs[i] = s, addr
				resp, err := http.Get("http://" + addr + "/ping")
				if err != nil {
					answers[i] = err.Error()
					return
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				answers[i] = string(b)
			}()
		}
		wg.Wait()
		for i := range srvs {
			if want := fmt.Sprintf("server-%d-%d", round, i); answers[i] != want {
				t.Errorf("round %d: server %d at %s answered %q, want %q (its own /ping)", round, i, addrs[i], answers[i], want)
			}
		}
		if addrs[0] == addrs[1] {
			t.Errorf("round %d: both starters ended on %s: one of the two servers failed to bind it and the other's /ping answered for it", round, addrs[0])
		}
		// Free the port for the next round: a pick that finds it still held gets
		// a fresh one and the race would not recur.
		for _, s := range srvs {
			if s != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.Shutdown(ctx)
				cancel()
			}
		}
		if t.Failed() {
			return
		}
	}
}

// A readiness probe answered by another server must not count: with a non-empty
// want, waitReadyErr takes only the body of the server's own /ping, and returns
// Start's error when that arrives instead.
func TestWaitReadyErrDoesNotTakeAnotherServersAnswer1104(t *testing.T) {
	other := http.NewServeMux()
	other.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok-other") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: other}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	bindErr := fmt.Errorf("listen tcp %s: bind: address already in use", ln.Addr())
	done := make(chan error, 1)
	go func() { time.Sleep(100 * time.Millisecond); done <- bindErr }()
	ok, err := waitReadyErr(&http.Client{Timeout: 300 * time.Millisecond}, "http://"+ln.Addr().String()+"/ping", "ok-mine", done)
	if ok || err == nil || !isAddrInUse(err) {
		t.Fatalf("waitReadyErr = %v, %v; want not ready and Start's error (the other server's answer is not this one's)", ok, err)
	}
	// Its own answer is ready.
	ok, err = waitReadyErr(&http.Client{Timeout: 300 * time.Millisecond}, "http://"+ln.Addr().String()+"/ping", "ok-other", make(chan error))
	if !ok || err != nil {
		t.Fatalf("waitReadyErr with the server's own body = %v, %v; want ready", ok, err)
	}
}
