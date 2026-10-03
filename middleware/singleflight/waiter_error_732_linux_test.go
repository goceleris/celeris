//go:build linux

package singleflight

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestWaiterErrorAndPanicSurviveLeaderNextRequest pins the rest of the
// singleflight site of celeris#732: what a waiter gets when the leader's
// handler fails.
//
// Every waiter returns the leader's error, or panics with the leader's panic
// value. Either can hold the leader's request strings (errors.New(c.Param(..)),
// an HTTPError whose Message is a header, panic(c.Header(..))), and on epoll
// and io_uring (and Adaptive, which runs them) those are views of the leader
// connection's receive buffer. The leader returns once it has handed the
// entry over and its connection's next requests are received into that
// buffer, while the waiter formats the error later, as its logger, its span
// or its recovery middleware would: here an outer middleware formats it
// after the leader's connection has sent two more requests (an async
// handler's request sits in a double buffer, so it takes two) with the same
// layout. The waiter must read the leader's first request. The HTTPError
// mode also checks what the waiter's error promises: errors.As finds an
// HTTPError with the leader's Message, and errors.Is finds the error it
// wraps.
func TestWaiterErrorAndPanicSurviveLeaderNextRequest(t *testing.T) {
	errWrapped := errors.New("wrapped sentinel")
	modes := []struct {
		name string
		fail func(c *celeris.Context) error // returns the leader's error, or panics
		want string
	}{
		{"error", func(c *celeris.Context) error { return errors.New(c.Header("x-err")) }, "err-aaaa"},
		{"httperror", func(c *celeris.Context) error {
			return celeris.NewHTTPError(400, c.Header("x-err")).WithError(errWrapped)
		}, "code=400, message=err-aaaa, err=wrapped sentinel|as=400 err-aaaa|is=true"},
		{"panic-string", func(c *celeris.Context) error { panic(c.Header("x-err")) }, "panic err-aaaa"},
		{"panic-error", func(c *celeris.Context) error { panic(errors.New(c.Header("x-err"))) }, "panic err-aaaa"},
	}
	// report renders what the waiter's outer middleware sees of the error.
	report := func(err error) string {
		s := err.Error()
		var he *celeris.HTTPError
		if errors.As(err, &he) {
			s += "|as=" + strconv.Itoa(he.Code) + " " + he.Message + "|is=" + strconv.FormatBool(errors.Is(err, errWrapped))
		}
		return s
	}

	for _, m := range modes {
		for _, a := range sfArms(t) {
			t.Run(m.name+"/"+a.name, func(t *testing.T) {
				joined := make(chan struct{}, 1)
				prev := testHookWaiterJoined
				testHookWaiterJoined = func() { joined <- struct{}{} }
				t.Cleanup(func() { testHookWaiterJoined = prev })
				leaderIn := make(chan struct{}, 1)
				gate, release := sfGate()
				gotCh := make(chan string, 1)

				addr, stop := sfStartServer(t, func() *celeris.Server {
					srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
					answer := func(c *celeris.Context) error {
						if !c.IsWritten() {
							_ = c.String(200, "ok")
							_ = c.FlushResponse()
						}
						return nil
					}
					// Answers every request with 200, and reports what the
					// waiter got once the gate opens.
					srv.Use(func(c *celeris.Context) (ret error) {
						waiter := c.Header("x-role") == "w"
						defer func() {
							if r := recover(); r != nil {
								if waiter {
									<-gate
									gotCh <- "panic " + fmt.Sprint(r)
								}
								ret = answer(c)
							}
						}()
						if err := c.Next(); err != nil && waiter {
							<-gate
							gotCh <- report(err)
						}
						return answer(c)
					})
					srv.Use(New(Config{KeyFunc: func(*celeris.Context) string { return "one-key" }}))
					srv.GET("/sf/:id", func(c *celeris.Context) error {
						if c.Header("x-hold") == "1" {
							leaderIn <- struct{}{}
							select {
							case <-joined:
							case <-time.After(10 * time.Second):
							}
							_ = c.String(200, "leader")
							return m.fail(c)
						}
						return c.String(200, "ok")
					}).Async()
					return srv
				})
				defer stop()
				// Before stop, which waits for the waiter's handler.
				defer release()

				lconn, lbr := sfDial(t, addr)
				defer func() { _ = lconn.Close() }()
				wconn, wbr := sfDial(t, addr)
				defer func() { _ = wconn.Close() }()

				// The leader's first request, held until the waiter joins.
				sfWrite(t, lconn, "GET /sf/aaaa HTTP/1.1\r\nHost: h\r\nX-Role: l\r\nX-Hold: 1\r\nX-Err: err-aaaa\r\n\r\n")
				select {
				case <-leaderIn:
				case <-time.After(10 * time.Second):
					t.Fatal("the leader's handler did not start")
				}
				sfWrite(t, wconn, "GET /sf/zzzz HTTP/1.1\r\nHost: h\r\nX-Role: w\r\nX-Hold: 0\r\nX-Err: err-zzzz\r\n\r\n")
				sfReadAny(t, lbr)
				for _, v := range []string{"bbbb", "cccc"} {
					sfWrite(t, lconn, "GET /sf/"+v+" HTTP/1.1\r\nHost: h\r\nX-Role: l\r\nX-Hold: 0\r\nX-Err: err-"+v+"\r\n\r\n")
					sfReadAny(t, lbr)
				}
				release()
				var got string
				select {
				case got = <-gotCh:
				case <-time.After(10 * time.Second):
					t.Fatal("the waiter did not report the leader's error or panic (did it coalesce?)")
				}
				sfReadAny(t, wbr)
				t.Logf("MW732SFERR mode=%s arm=%s waiter saw %q (want %q)", m.name, a.name, got, m.want)
				if got != m.want {
					t.Errorf("the waiter's copy of the leader's %s reads %q after the leader's connection sent its next requests; want %q", m.name, got, m.want)
				}
			})
		}
	}
}

// sfReadAny reads one response of any status.
func sfReadAny(t *testing.T, br *bufio.Reader) {
	t.Helper()
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read status: %v", err)
	}
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if _, err := br.Discard(n); err != nil {
		t.Fatalf("read body: %v", err)
	}
}
