//go:build linux

package memcached

// Client.Close from another goroutine while commands are in flight or being issued: every command ends
// with an error that is ErrClosed (never success after the close took the conn, never the internal
// errors of the event loop or the pool: "celeris/eventloop: loop is closed", "celeris/async: pool is
// closed", "use of closed network connection"). Text and binary x the loop path, a started std server
// and the direct path. The fake answers every key with "hello": what the test asserts is the error.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
)

// answerServer answers every request in full (a binary handshake first), or, with hold set, reads the
// requests and never answers; inflight counts the requests it has read.
func answerServer(t *testing.T, bin, hold bool, inflight *atomic.Int64) string {
	t.Helper()
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
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				buf := make([]byte, 64<<10)
				if bin {
					if _, err := io.ReadFull(c, buf[:24]); err != nil {
						return
					}
					_, _ = c.Write(binPacket(0x0b, buf[12:16], nil, nil, []byte("1.6.41")))
				}
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if inflight != nil {
						inflight.Add(1)
					}
					if hold {
						continue
					}
					var reply []byte
					if bin {
						reply, _ = binReply(buf[:n])
					} else {
						reply, _ = textReply(string(buf[:n]))
					}
					if _, err := c.Write(reply); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func closeTargets(t *testing.T) []struct {
	name string
	opts func() []Option
} {
	std := looptest.StartStd(t)
	return []struct {
		name string
		opts func() []Option
	}{
		{"loop", func() []Option { return []Option{WithEngine(looptest.Unstarted())} }},
		{"std", func() []Option { return []Option{WithEngine(std)} }},
		{"direct", func() []Option { return nil }},
	}
}

// Commands in flight (the server has read them and never answers) when Client.Close runs. On the
// event-loop paths Client.Close ends them with ErrClosed. A direct conn is checked out of the pool
// and Client.Close closes the idle ones only: the command ends by its own reply or limit (here the
// 500 ms per-operation limit, a deadline error): what it must never do is return success.
func TestClientCloseFailsTheCommandsInFlight(t *testing.T) {
	for _, proto := range []Protocol{ProtocolText, ProtocolBinary} {
		for _, p := range closeTargets(t) {
			for _, cmd := range []string{"Get", "Set"} {
				t.Run(fmt.Sprintf("%v/%s/%s", proto, p.name, cmd), func(t *testing.T) {
					var inflight atomic.Int64
					addr := answerServer(t, proto == ProtocolBinary, true, &inflight)
					limit := 20 * time.Second
					if p.name == "direct" {
						limit = 500 * time.Millisecond
					}
					c, err := NewClient(addr, append([]Option{WithProtocol(proto), WithDialTimeout(2 * time.Second), WithTimeout(limit)}, p.opts()...)...)
					if err != nil {
						t.Fatal(err)
					}
					const n = 8
					errs := make(chan error, n)
					for i := 0; i < n; i++ {
						go func(i int) {
							ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
							defer cancel()
							k := fmt.Sprintf("close-%s-%d", cmd, i)
							if cmd == "Get" {
								_, err := c.Get(ctx, k)
								errs <- err
							} else {
								errs <- c.Set(ctx, k, "v", time.Minute)
							}
						}(i)
					}
					deadline := time.Now().Add(5 * time.Second)
					for inflight.Load() < n {
						if time.Now().After(deadline) {
							t.Fatalf("only %d of %d commands reached the server", inflight.Load(), n)
						}
						time.Sleep(time.Millisecond)
					}
					_ = c.Close()
					for i := 0; i < n; i++ {
						select {
						case err := <-errs:
							if !errors.Is(err, ErrClosed) && !(p.name == "direct" && errors.Is(err, context.DeadlineExceeded)) {
								t.Errorf("%s in flight during Client.Close returned %v, want ErrClosed", cmd, err)
							}
						case <-time.After(8 * time.Second):
							t.Fatalf("%s in flight during Client.Close did not return", cmd)
						}
					}
					// and after the close
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if _, err := c.Get(ctx, "after"); !errors.Is(err, ErrClosed) {
						t.Errorf("Get after Client.Close returned %v, want ErrClosed", err)
					}
				})
			}
		}
	}
}

// Commands issued back to back by 8 goroutines while Client.Close runs at a random moment.
func TestClientCloseWhileCommandsRun(t *testing.T) {
	for _, proto := range []Protocol{ProtocolText, ProtocolBinary} {
		for _, p := range closeTargets(t) {
			t.Run(fmt.Sprintf("%v/%s", proto, p.name), func(t *testing.T) {
				addr := answerServer(t, proto == ProtocolBinary, false, nil)
				for it := 0; it < 15; it++ {
					c, err := NewClient(addr, append([]Option{WithProtocol(proto), WithDialTimeout(2 * time.Second), WithTimeout(20 * time.Second)}, p.opts()...)...)
					if err != nil {
						t.Fatal(err)
					}
					var wg sync.WaitGroup
					var bad atomic.Int64
					var first atomic.Value
					for g := 0; g < 8; g++ {
						wg.Add(1)
						go func(g int) {
							defer wg.Done()
							ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
							defer cancel()
							for i := 0; ; i++ {
								k := fmt.Sprintf("close-run-%d-%d", g, i)
								var err error
								if i%2 == 0 {
									_, err = c.Get(ctx, k)
								} else {
									err = c.Set(ctx, k, "v", time.Minute)
								}
								if err != nil {
									if !errors.Is(err, ErrClosed) {
										bad.Add(1)
										first.CompareAndSwap(nil, err.Error())
									}
									return
								}
							}
						}(g)
					}
					time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
					_ = c.Close()
					done := make(chan struct{})
					go func() { wg.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Fatal("commands did not end after Client.Close")
					}
					if bad.Load() != 0 {
						t.Fatalf("iteration %d: %d commands ended with an error that is not ErrClosed, e.g. %v", it, bad.Load(), first.Load())
					}
				}
			})
		}
	}
}
