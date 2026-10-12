//go:build linux

package celeris_test

import (
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Tests for the header budget across protocol detection. ReadHeaderTimeout is
// one budget for the first request: it counts from accept, as on std, and
// detecting the protocol does not start it again. Before, the engines armed
// a fresh full header deadline at detection (initProtocol), so a client that
// sent "GET", waited most of the budget and then trickled the headers held the
// conn for nearly twice ReadHeaderTimeout: 540 to 570 ms at 300 ms measured on
// io_uring and io_uring-async, against 300 to 310 ms on std (probe of the
// review of PR 975, probes/zz_budget_probe_974_linux_test.go).

// budgetEngines are the engines with a pre-detection state, plus std as the
// control for the bound.
var budgetEngines = []engine974{{"std", celeris.Std, false, false}, {"epoll", celeris.Epoll, false, false}, {"io_uring", celeris.IOUring, false, false},
	{"epoll-async", celeris.Epoll, true, false}, {"io_uring-async", celeris.IOUring, true, false}, {"io_uring-mshot", celeris.IOUring, false, true}}

// TestHeaderBudgetSpansDetection: "GET" decides nothing (fewer than the four
// bytes detection needs), the client stalls, finishes the request line and
// keeps the header block open with a trickle. The conn must close at the
// ReadHeaderTimeout counted from accept, not from detection.
func TestHeaderBudgetSpansDetection(t *testing.T) {
	scenarios := []struct {
		name  string
		stall time.Duration
		every time.Duration
		chunk string
	}{
		{"line-every-50ms-after-240ms", 240 * time.Millisecond, 50 * time.Millisecond, "X-Slow: a\r\n"},
		{"byte-every-20ms-after-270ms", 270 * time.Millisecond, 20 * time.Millisecond, "X"},
	}
	// 300 ms of budget; a close within 150 ms of it is the budget counted
	// from accept, and one at 540 ms or later is a budget that restarted at
	// detection.
	const bound = readHeaderTimeout974 + 150*time.Millisecond
	for _, e := range budgetEngines {
		for _, sc := range scenarios {
			t.Run(e.name+"/"+sc.name, func(t *testing.T) {
				e.env974(t)
				addr := startAuto974(t, e.eng, func(c *celeris.Config) {
					c.AsyncHandlers = e.async
					c.ReadHeaderTimeout = readHeaderTimeout974
					c.ReadTimeout = 5 * time.Minute
					c.IdleTimeout = 5 * time.Minute
				})
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				start := time.Now()
				res := watchClose974(c, closeWithin974)
				_, _ = io.WriteString(c, "GET")
				time.Sleep(sc.stall)
				_, _ = io.WriteString(c, " / HTTP/1.1\r\nHost: x\r\n")
				tick := time.NewTicker(sc.every)
				defer tick.Stop()
				for !res.finished() {
					<-tick.C
					if _, err := io.WriteString(c, sc.chunk); err != nil {
						break
					}
				}
				r := res.wait()
				took := r.at.Sub(start)
				t.Logf("BUDGET %s: closed=%v (%v) after %v (ReadHeaderTimeout %v, bound %v)", t.Name(), r.closed, r.err, took.Round(10*time.Millisecond), readHeaderTimeout974, bound)
				if !r.closed {
					t.Fatalf("the conn was not closed within %v: read ended with %v", closeWithin974, r.err)
				}
				if took > bound {
					t.Errorf("the conn was held %v with ReadHeaderTimeout %v (bound %v): the header budget restarted at protocol detection",
						took.Round(10*time.Millisecond), readHeaderTimeout974, bound)
				}
			})
		}
	}
}

// TestRequestCompletedAfterTheBudgetIsNotServed: "GE" is sent at once and the
// rest of a complete request only after ReadHeaderTimeout has passed. The
// deciding read arrives with the budget already spent, so the conn is closed
// and the request is not answered. The io_uring sweep is 800 ms apart on an
// idle worker, so this read can be the first the engine sees past the
// deadline, and the clamp must close there (the "io_uring-mshot" arm also
// checks that the provided buffer goes back to the ring on that exit).
func TestRequestCompletedAfterTheBudgetIsNotServed(t *testing.T) {
	for _, e := range budgetEngines {
		t.Run(e.name, func(t *testing.T) {
			e.env974(t)
			addr := startAuto974(t, e.eng, func(c *celeris.Config) {
				c.AsyncHandlers = e.async
				c.ReadHeaderTimeout = readHeaderTimeout974
				c.ReadTimeout = 5 * time.Minute
				c.IdleTimeout = 5 * time.Minute
			})
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_, _ = io.WriteString(c, "GE")
			time.Sleep(readHeaderTimeout974 + 50*time.Millisecond)
			_, _ = io.WriteString(c, "T /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			_ = c.SetReadDeadline(time.Now().Add(closeWithin974))
			b, _ := io.ReadAll(c)
			t.Logf("%s: read back %q", t.Name(), b)
			if strings.HasPrefix(string(b), "HTTP/1.1 200") {
				t.Errorf("a request completed %v after accept, at ReadHeaderTimeout %v, was answered: %q",
					readHeaderTimeout974+50*time.Millisecond, readHeaderTimeout974, b)
			}
		})
	}
}

// TestSpentBudgetReturnsItsRingBuffer: with multishot recv the deciding recv of
// a conn arrives in a buffer of the worker's provided-buffer ring, and the
// close of a conn whose budget is spent (the exit TestRequestCompletedAfterTheBudgetIsNotServed
// takes) has to give the buffer back to the ring. One that is dropped is gone
// for good. A worker's ring holds at least 1024 buffers (bufRingCountMin, the
// size this test pins with CELERIS_IOURING_PBUF_COUNT), so the test sends three
// rings' worth of such conns: whatever the split between workers, each is past
// its ring. A GET on a new conn is then answered.
func TestSpentBudgetReturnsItsRingBuffer(t *testing.T) {
	const (
		ringSize = 1024 // bufRingCountMin
		lateConn = 3 * ringSize
		parallel = 256
	)
	t.Setenv("CELERIS_IOURING_PBUF_COUNT", strconv.Itoa(ringSize))
	engine974{"io_uring-mshot", celeris.IOUring, false, true}.env974(t)
	addr := startAuto974(t, celeris.IOUring, func(c *celeris.Config) {
		c.Workers = 2
		c.ReadHeaderTimeout = readHeaderTimeout974
		c.ReadTimeout = 5 * time.Minute
		c.IdleTimeout = 5 * time.Minute
	})
	var answered, closed atomic.Int64
	sem := make(chan struct{}, parallel)
	done := make(chan struct{}, lateConn)
	for i := 0; i < lateConn; i++ {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; done <- struct{}{} }()
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			_, _ = io.WriteString(c, "GE")
			time.Sleep(readHeaderTimeout974 + 50*time.Millisecond)
			_, _ = io.WriteString(c, "T /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			_ = c.SetReadDeadline(time.Now().Add(closeWithin974))
			b, _ := io.ReadAll(c)
			if strings.HasPrefix(string(b), "HTTP/1.1 200") {
				answered.Add(1)
			} else {
				closed.Add(1)
			}
		}()
	}
	for i := 0; i < lateConn; i++ {
		<-done
	}
	t.Logf("%d of %d conns completed their request after the budget: %d answered, %d closed unanswered", answered.Load()+closed.Load(), lateConn, answered.Load(), closed.Load())
	if answered.Load() != 0 {
		t.Errorf("%d requests completed after ReadHeaderTimeout were answered", answered.Load())
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(b), "HTTP/1.1 200") || !strings.HasSuffix(string(b), "ok") {
		t.Errorf("a GET after %d conns closed on a spent budget got %q, want a 200 answer: a worker's ring ran out of buffers", lateConn, b)
	}
}
