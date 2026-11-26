//go:build redis_delay_hook && linux

package redis

// A failed Pipeline.Exec leaves the dead connection's reader able to write a
// late reply into a request of that batch (the event loop allows one more
// read after the close). If the Pipeline then reuses those requests (a retry
// as it is, or Release and the next Client.Pipeline() of any goroutine) the
// late write lands in the new batch: a command receives the reply of another
// batch or goroutine.
//
// These tests widen that window with the dispatch hook (build tag
// redis_delay_hook: one dispatch in ten pauses for LATE_DELAY after taking a
// request) and give every command its own key, the fake server answering
// "val-<key>": any reply that reaches the wrong command is a wrong value.
//
//	go test -tags redis_delay_hook [-race] -run TestFailedExecLateWriter ./driver/redis/
//
// Env: LATE_SECS (seconds per case, default 3), LATE_DELAYS_US (default
// "300,5000").

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/busypolltest"
)

type lateCounts struct {
	batches, failed, retries, okChecks, wrong, hangs, panics atomic.Int64
	mu                                                       sync.Mutex
	samples                                                  []string
}

func (c *lateCounts) bad(format string, a ...any) {
	if c.wrong.Add(1) <= 8 {
		c.mu.Lock()
		c.samples = append(c.samples, fmt.Sprintf(format, a...))
		c.mu.Unlock()
	}
}

// lateServer answers GET with "val-<key>" after 0 to 700 microseconds and
// supports MULTI/EXEC of GETs (queued per connection, found by its writer).
func lateServer(t *testing.T) *fakeRedis {
	var mu sync.Mutex
	queued := map[*bufio.Writer][]string{}
	return startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "MULTI":
			mu.Lock()
			queued[w] = []string{}
			mu.Unlock()
			writeSimple(w, "OK")
		case "GET":
			mu.Lock()
			q, inMulti := queued[w]
			if inMulti {
				queued[w] = append(q, cmd[1])
			}
			mu.Unlock()
			if inMulti {
				writeSimple(w, "QUEUED")
				return
			}
			time.Sleep(time.Duration(rand.IntN(700)) * time.Microsecond)
			writeBulk(w, "val-"+cmd[1])
		case "EXEC":
			mu.Lock()
			q := queued[w]
			delete(queued, w)
			mu.Unlock()
			time.Sleep(time.Duration(rand.IntN(700)) * time.Microsecond)
			writeArrayHeader(w, len(q))
			for _, k := range q {
				writeBulk(w, "val-"+k)
			}
		}
	})
}

// pipelineLate runs workers that pipeline per-command keys under a short
// random deadline. shape "retry": a failed Exec is retried on the same
// Pipeline with new keys and no Discard; shape "release": the failed Pipeline
// is Released and the next batch takes a Pipeline from the pool.
func pipelineLate(t *testing.T, c *Client, shape string, secs int, cnt *lateCounts) {
	stop := time.Now().Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					cnt.panics.Add(1)
					cnt.bad("panic: %v", r)
				}
			}()
			for b := 0; time.Now().Before(stop); b++ {
				cnt.batches.Add(1)
				p := c.Pipeline()
				queue := func(try int) ([]*StringCmd, []string) {
					keys := make([]string, 3)
					cmds := make([]*StringCmd, 3)
					for i := range keys {
						keys[i] = fmt.Sprintf("g%d-b%d-t%d-c%d", g, b, try, i)
						cmds[i] = p.Get(keys[i])
					}
					return cmds, keys
				}
				check := func(cmds []*StringCmd, keys []string) {
					for i, cmd := range cmds {
						v, e := cmd.Result()
						switch {
						case e != nil:
							cnt.bad("%s: error %v after a nil Exec", keys[i], e)
						case v != "val-"+keys[i]:
							cnt.bad("%s: got %q", keys[i], v)
						default:
							cnt.okChecks.Add(1)
						}
					}
				}
				cmds, keys := queue(0)
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(200+rand.IntN(900))*time.Microsecond)
				err := p.Exec(ctx)
				cancel()
				if err == nil {
					check(cmds, keys)
					p.Release()
					continue
				}
				cnt.failed.Add(1)
				if shape == "release" {
					p.Release()
					continue
				}
				for try := 1; try <= 20; try++ {
					cnt.retries.Add(1)
					cmds, keys = queue(try)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					st := time.Now()
					err = p.Exec(ctx)
					cancel()
					if err == nil {
						check(cmds, keys)
						break
					}
					if time.Since(st) > 2*time.Second {
						cnt.hangs.Add(1)
					}
				}
				p.Release()
			}
		}(g)
	}
	wg.Wait()
}

// singleAndTxLate runs single commands and Tx on the client under the same
// deadlines, checking every value against its key.
func singleAndTxLate(t *testing.T, c *Client, secs int, cnt *lateCounts) {
	stop := time.Now().Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; time.Now().Before(stop); n++ {
				key := fmt.Sprintf("g%d-n%d", g, n)
				cnt.batches.Add(1)
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(200+rand.IntN(900))*time.Microsecond)
				if n%2 == 0 {
					v, err := c.Get(ctx, key)
					switch {
					case err != nil:
						cnt.failed.Add(1)
					case v != "val-"+key:
						cnt.bad("single %s: got %q", key, v)
					default:
						cnt.okChecks.Add(1)
					}
				} else {
					tx, err := c.TxPipeline(ctx)
					if err == nil {
						g1 := tx.Get(key + "-a")
						g2 := tx.Get(key + "-b")
						if err = tx.Exec(ctx); err != nil {
							cnt.failed.Add(1)
						} else {
							for _, x := range []struct {
								cmd *StringCmd
								key string
							}{{g1, key + "-a"}, {g2, key + "-b"}} {
								if v, e := x.cmd.Result(); e != nil || v != "val-"+x.key {
									cnt.bad("tx %s: got %q, %v", x.key, v, e)
								} else {
									cnt.okChecks.Add(1)
								}
							}
						}
						_ = tx.Discard()
					} else {
						cnt.failed.Add(1)
					}
				}
				cancel()
			}
		}(g)
	}
	wg.Wait()
}

func lateDelays(t *testing.T) []time.Duration {
	spec := os.Getenv("LATE_DELAYS_US")
	if spec == "" {
		spec = "300,5000"
	}
	var out []time.Duration
	for _, f := range strings.Split(spec, ",") {
		us, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || us <= 0 {
			t.Fatalf("LATE_DELAYS_US: %q", f)
		}
		out = append(out, time.Duration(us)*time.Microsecond)
	}
	return out
}

func lateSecs() int {
	if v, err := strconv.Atoi(os.Getenv("LATE_SECS")); err == nil && v > 0 {
		return v
	}
	return 3
}

func report(t *testing.T, label string, cnt *lateCounts, hooks uint64) {
	t.Helper()
	line := fmt.Sprintf("LATEWRITER %s batches=%d failed=%d retries=%d okChecks=%d WRONG=%d hangs=%d panics=%d hookCalls=%d",
		label, cnt.batches.Load(), cnt.failed.Load(), cnt.retries.Load(), cnt.okChecks.Load(), cnt.wrong.Load(), cnt.hangs.Load(), cnt.panics.Load(), hooks)
	fmt.Println(line)
	for _, s := range cnt.samples {
		fmt.Println("LATEWRITER wrong:", s)
	}
	if cnt.wrong.Load() > 0 || cnt.hangs.Load() > 0 || cnt.panics.Load() > 0 {
		t.Errorf("%s: %d wrong values, %d hangs, %d panics", label, cnt.wrong.Load(), cnt.hangs.Load(), cnt.panics.Load())
	}
}

// TestFailedExecLateWriterStandalone: the standalone client (no WithEngine),
// where the event-loop worker reads the connection, in both shapes at each
// delay.
func TestFailedExecLateWriterStandalone(t *testing.T) {
	fake := lateServer(t)
	c, err := NewClient(fake.Addr(), WithPoolSize(64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, delay := range lateDelays(t) {
		for _, shape := range []string{"retry", "release"} {
			t.Run(fmt.Sprintf("%s/%dus", shape, delay.Microseconds()), func(t *testing.T) {
				dispatchDelay.Store(int64(delay))
				defer dispatchDelay.Store(0)
				before := dispatchCalls.Load()
				var cnt lateCounts
				pipelineLate(t, c, shape, lateSecs(), &cnt)
				report(t, fmt.Sprintf("standalone shape=%s delay=%dus", shape, delay.Microseconds()), &cnt, dispatchCalls.Load()-before)
			})
		}
	}
}

// TestFailedExecLateWriterNoExposure: the clients that have no late writer.
// The engine-backed client reads on the caller's goroutine; single commands
// and Tx on the standalone client take their requests from a pool that a
// failed wait never returns to. Same delays, same per-key checks: zero wrong
// values, with the hook calls counted to show the window was exercised.
func TestFailedExecLateWriterNoExposure(t *testing.T) {
	fake := lateServer(t)
	engine, err := NewClient(fake.Addr(), WithPoolSize(64), WithEngine(busypolltest.StartServer(t, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	standalone, err := NewClient(fake.Addr(), WithPoolSize(64))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = standalone.Close() })
	for _, delay := range lateDelays(t) {
		us := delay.Microseconds()
		t.Run(fmt.Sprintf("engine-pipeline/%dus", us), func(t *testing.T) {
			dispatchDelay.Store(int64(delay))
			defer dispatchDelay.Store(0)
			for _, shape := range []string{"retry", "release"} {
				before := dispatchCalls.Load()
				var cnt lateCounts
				pipelineLate(t, engine, shape, lateSecs(), &cnt)
				report(t, fmt.Sprintf("engine-backed shape=%s delay=%dus", shape, us), &cnt, dispatchCalls.Load()-before)
			}
		})
		t.Run(fmt.Sprintf("standalone-single-and-tx/%dus", us), func(t *testing.T) {
			dispatchDelay.Store(int64(delay))
			defer dispatchDelay.Store(0)
			before := dispatchCalls.Load()
			var cnt lateCounts
			singleAndTxLate(t, standalone, lateSecs(), &cnt)
			report(t, fmt.Sprintf("standalone single+Tx delay=%dus", us), &cnt, dispatchCalls.Load()-before)
		})
	}
}
