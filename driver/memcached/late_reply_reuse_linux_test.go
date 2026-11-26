//go:build linux

package memcached

// A request that failed on a conn the event loop still reads must not be handed
// to another command. The loop can run onRecv after the caller has given up
// (ctx deadline, cancel, close: the eventloop package documents it), so the
// worker may still be between taking the request a reply belongs to and writing
// the reply into it when the caller returns. If the caller put the request back
// in reqPool, the next getRequest, on any goroutine, receives an object a
// worker is about to write another command's reply into: Get returns the value
// of another key, GetMulti returns a key it was not asked for, or a command
// returns before its own reply is in.
//
// The probe models the descheduled worker with the shared pause of the event
// loop package (eventloop.SetDispatchPause): one dispatch in four sleeps, for
// 300 us or 5 ms, after taking its request and before writing the reply. Every
// key holds a value that names the key, so any reply that reaches a command
// that did not ask for it is told at once. It needs a conn the loop reads (a
// Client opened WithEngine of a server that has no loop of its own, on the
// driver's standalone loop; the default Client reads on the caller's goroutine
// and has nothing to race with, see TestDirectClientHasNoLateWriter).
//
// It runs against the in-process fake (text protocol) always, and against a
// real server, text and binary, when CELERIS_MEMCACHED_ADDR is set (the CI
// conformance servers; the PR notes run it on memcached 1.6.41 and 1.6.29).
// CELERIS_MC_REUSE_SECONDS sets how long each case runs (default 1).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/internal/looptest"
)

// startLatencyProxy listens on a loopback port and forwards every connection to
// upstream, holding each server-to-client chunk back for lat, so that a reply
// comes after the loop's synchronous poll window and the worker, not the caller,
// delivers it.
func startLatencyProxy(t testing.TB, upstream string, lat time.Duration) string {
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
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = c.Close()
				continue
			}
			go func() {
				_, _ = io.Copy(up, c)
				_ = up.Close()
			}()
			go func() {
				defer func() { _ = c.Close(); _ = up.Close() }()
				buf := make([]byte, 64<<10)
				for {
					n, err := up.Read(buf)
					if n > 0 {
						time.Sleep(lat)
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// proxyLatency is above the synchronous poll window of the loop (about 1 ms).
const proxyLatency = 1500 * time.Microsecond

type reuseTally struct {
	ops, failed                         atomic.Int64
	wrongValue, foreignKey, missingKeys atomic.Int64
	mu                                  sync.Mutex
	samples                             []string
}

func (r *reuseTally) bad(c *atomic.Int64, format string, args ...any) {
	c.Add(1)
	r.mu.Lock()
	if len(r.samples) < 6 {
		r.samples = append(r.samples, fmt.Sprintf(format, args...))
	}
	r.mu.Unlock()
}

func (r *reuseTally) violations() int64 {
	return r.wrongValue.Load() + r.foreignKey.Load() + r.missingKeys.Load()
}

// reuseShape is how the caller's command fails.
type reuseShape int

const (
	shapeDeadline reuseShape = iota // a ctx deadline that ends inside the paused dispatch
	shapeCancel                     // a cancel that arrives inside the paused dispatch
)

func (s reuseShape) String() string {
	if s == shapeCancel {
		return "cancel"
	}
	return "deadline"
}

// runLateReuse drives workers goroutines on one Client. Each owns keys whose
// values name them; each iteration fails one command on purpose (the shape) and
// then runs a command that must succeed, on the same goroutine, while the other
// goroutines do the same: a request put back by one is taken by the next
// getRequest of any.
func runLateReuse(t *testing.T, addr string, proto Protocol, shape reuseShape, delay, dur time.Duration) *reuseTally {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2)) // sync.Pool is per P: few Ps make cross-goroutine reuse common
	addr = startLatencyProxy(t, addr, proxyLatency)
	cl, err := NewClient(addr, WithEngine(looptest.Unstarted()), WithProtocol(proto),
		WithDialTimeout(2*time.Second), WithMaxOpen(64))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()

	const workers, perWorker = 8, 4
	uid := strconv.FormatInt(time.Now().UnixNano(), 36)
	keys := make([][]string, workers)
	setup := context.Background()
	for g := range keys {
		for j := 0; j < perWorker; j++ {
			k := fmt.Sprintf("mkr-%s-g%d-k%d", uid, g, j)
			keys[g] = append(keys[g], k)
			if err := cl.Set(setup, k, "val-"+k, time.Minute); err != nil {
				t.Fatalf("set %s: %v", k, err)
			}
		}
	}

	pause := eventloop.NewPause(4, delay)
	t.Cleanup(eventloop.SetDispatchPause(pause))

	tally := &reuseTally{}
	var wg sync.WaitGroup
	stop := time.Now().Add(dur)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			mine := keys[g]
			for i := 0; time.Now().Before(stop); i++ {
				// 1. The command that fails. Its reply is on its way (or being
				// dispatched, paused) when the context ends.
				var fctx context.Context
				var cancel context.CancelFunc
				var tm *time.Timer
				// The reply reaches the worker at about proxyLatency and, on one
				// dispatch in four, is held for delay: end the command inside it.
				d := proxyLatency + delay/5 + time.Duration(i%4)*delay/5
				switch shape {
				case shapeCancel:
					fctx, cancel = context.WithCancel(context.Background())
					tm = time.AfterFunc(d, cancel)
				default:
					fctx, cancel = context.WithTimeout(context.Background(), d)
				}
				mk := mine[i%perWorker]
				var ferr error
				switch i % 3 {
				case 0:
					_, ferr = cl.Get(fctx, mk)
				case 1:
					_, ferr = cl.GetMulti(fctx, mine[:3]...)
				default:
					_, ferr = cl.GetBytes(fctx, mk)
				}
				cancel()
				if tm != nil {
					tm.Stop()
				}
				if ferr != nil {
					tally.failed.Add(1)
				}
				// 2. The commands that must be right, at once, on this goroutine.
				for n := 0; n < 2; n++ {
					checkReuse(tally, cl, mine, i+n)
				}
			}
		}(g)
	}
	wg.Wait()
	t.Logf("%v/%v/%v: ops=%d failed-on-purpose=%d pauses=%d/%d dispatches: wrong-value=%d foreign-key=%d missing=%d",
		proto, shape, delay, tally.ops.Load(), tally.failed.Load(), pause.Hits(), pause.Calls(),
		tally.wrongValue.Load(), tally.foreignKey.Load(), tally.missingKeys.Load())
	for _, s := range tally.samples {
		t.Log("  sample: " + s)
	}
	if pause.Hits() == 0 || tally.failed.Load() == 0 {
		t.Fatalf("the probe did not reach its window (pauses=%d failures=%d)", pause.Hits(), tally.failed.Load())
	}
	return tally
}

// checkReuse runs one command that has to succeed with its own keys' values.
func checkReuse(tally *reuseTally, cl *Client, mine []string, i int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tally.ops.Add(1)
	switch i % 2 {
	case 0:
		k := mine[i%len(mine)]
		got, err := cl.Get(ctx, k)
		switch {
		case err == nil && got != "val-"+k:
			tally.bad(&tally.wrongValue, "Get(%s) = %q", k, got)
		case errors.Is(err, ErrCacheMiss):
			tally.bad(&tally.missingKeys, "Get(%s) = miss: the reply never came, a late finish() ended the command", k)
		}
	default:
		ask := mine[:3]
		got, err := cl.GetMulti(ctx, ask...)
		if err != nil {
			return
		}
		want := map[string]bool{}
		for _, k := range ask {
			want[k] = true
		}
		for k, v := range got {
			if !want[k] {
				tally.bad(&tally.foreignKey, "GetMulti(%v) returned the key %s it was not asked for", ask, k)
			} else if v != "val-"+k {
				tally.bad(&tally.wrongValue, "GetMulti: %s = %q", k, v)
			}
		}
		if len(got) < len(ask) {
			tally.bad(&tally.missingKeys, "GetMulti(%v) returned %d of %d keys: a late finish() ended the command", ask, len(got), len(ask))
		}
	}
}

func reuseDuration() time.Duration {
	if s := os.Getenv("CELERIS_MC_REUSE_SECONDS"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return time.Second
}

func TestLateReplyDoesNotReachAReusedRequest(t *testing.T) {
	type target struct {
		name  string
		addr  string
		proto Protocol
	}
	var targets []target
	fake := startFake(t)
	targets = append(targets, target{"fake/text", fake.Addr(), ProtocolText})
	if real := strings.TrimSpace(os.Getenv("CELERIS_MEMCACHED_ADDR")); real != "" {
		targets = append(targets,
			target{"real/text", real, ProtocolText},
			target{"real/binary", real, ProtocolBinary})
	}
	delays := []time.Duration{300 * time.Microsecond, 5 * time.Millisecond}
	for _, tg := range targets {
		for _, shape := range []reuseShape{shapeDeadline, shapeCancel} {
			for _, d := range delays {
				t.Run(fmt.Sprintf("%s/%v/%v", tg.name, shape, d), func(t *testing.T) {
					tally := runLateReuse(t, tg.addr, tg.proto, shape, d, reuseDuration())
					if v := tally.violations(); v != 0 {
						t.Fatalf("%d replies reached a command that did not ask for them (wrong value %d, foreign key %d, missing %d)",
							v, tally.wrongValue.Load(), tally.foreignKey.Load(), tally.missingKeys.Load())
					}
				})
			}
		}
	}
}
