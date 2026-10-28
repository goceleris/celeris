//go:build linux || darwin

package redis

// A Do whose context ends while a fresh conn's HELLO reply is arriving: exec
// returns the context's error, but the reader goroutine already popped the
// request and is filling it in (state.go dispatch). handshake gave that request
// back to the pool on every failed exec, so another Do could take the same
// object, and the reader's late finish woke it before its own reply came: an
// empty reply with a nil error on a healthy conn, and -race names the two
// writers (found by the review of celeris#887 / PR #1154; the code is from
// April, 5cb0f9b).
//
// The window is the microseconds around the context's end, so the cell is a
// stress test: 1500 rounds with random delays and timeouts, and it is only
// meaningful under -race (-count=20 is the bar; without -race it counts wrong
// replies, which is probabilistic). It is left out of the Windows job, whose
// timer granularity would make it pass without testing anything.

import (
	"bufio"
	"context"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandshakeWhoseContextEndedDoesNotRecycleItsRequestWhileTheReaderHoldsIt887(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				w := bufio.NewWriter(c)
				for {
					cmd, err := readCommandInto(r, nil)
					if err != nil {
						return
					}
					switch strings.ToUpper(cmd[0]) {
					case "HELLO":
						time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
						handleHELLO(w, 3)
					case "PING":
						writeSimple(w, "PONG")
					default:
						writeSimple(w, "OK")
					}
					if w.Flush() != nil {
						return
					}
				}
			}(c)
		}
	}()
	addr := ln.Addr().String()

	// A healthy client whose PING replies are checked while the others'
	// handshakes are cut short: a recycled request wakes one of them with a
	// reply that is not its own.
	checker, err := NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checker.Close() })
	var bad atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v, err := checker.Do(context.Background(), "PING")
				if err != nil {
					// (A dial error: the loopback runs out of ports under this
					// churn on macOS. Only a reply that is not the conn's own
					// counts.)
					continue
				}
				if v == nil || string(v.Str) != "PONG" {
					if bad.Add(1) <= 3 {
						t.Logf("the checker's PING got v=%+v, want PONG", v)
					}
				}
			}
		}()
	}
	for i := 0; i < 1500; i++ {
		c, err := NewClient(addr)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(50+rand.IntN(300))*time.Microsecond)
		_, _ = c.Do(ctx, "PING")
		cancel()
		_ = c.Close()
	}
	close(stop)
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Errorf("%d PINGs on a healthy conn got a reply that was not theirs: a request of a failed handshake was recycled while its reader still held it", n)
	}
}
