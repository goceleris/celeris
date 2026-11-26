package redis

// The harsh setup for the Pipeline: large values (24 KiB, so a reply spans
// several reads), tight deadlines (20 to 420 microseconds) on half the
// batches, immediate reuse (a failed Pipeline is retried as it is or
// Released and the next Client.Pipeline() takes its buffers at once),
// per-command markers (every command has its own key and its own expected
// 24 KiB value, so a reply that reaches the wrong command, batch or
// goroutine is detected by content), no instrument, 8 goroutines. Both the
// standalone client (the event-loop worker reads the connection: the one
// where a failed Exec's connection can still be delivering a late reply) and,
// on Linux, an engine-backed client for comparison.
//
// Short by default (about a second per mode); a long run:
//
//	HARSH_SECS=120 go test -race -run TestPipelineHarsh -v ./driver/redis/
//
// A command whose Result is not an error must carry its own value; after a
// failed Exec every command of the batch must report an error; nothing may
// panic or hang.

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
)

const harshValueLen = 24 << 10

// A key's value is the key and a separator repeated up to harshValueLen bytes.

// writeHarshValue writes key's value as a bulk string without building it.
func writeHarshValue(w *bufio.Writer, key string) {
	_, _ = w.WriteString("$" + strconv.Itoa(harshValueLen) + "\r\n")
	unit := key + "|"
	for n := 0; n < harshValueLen; {
		k := harshValueLen - n
		if k > len(unit) {
			k = len(unit)
		}
		_, _ = w.WriteString(unit[:k])
		n += k
	}
	_, _ = w.WriteString("\r\n")
}

// isHarshValue reports whether v is key's value, without building it.
func isHarshValue(v, key string) bool {
	if len(v) != harshValueLen {
		return false
	}
	unit := key + "|"
	for n := 0; n < harshValueLen; {
		k := harshValueLen - n
		if k > len(unit) {
			k = len(unit)
		}
		if v[n:n+k] != unit[:k] {
			return false
		}
		n += k
	}
	return true
}

func harshServer(t *testing.T) *fakeRedis {
	return startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
			writeHarshValue(w, cmd[1])
		}
	})
}

type harshCounts struct {
	batches, commands, failed, retries, checked, wrong, afterFail, hangs, panics atomic.Int64
	mu                                                                           sync.Mutex
	samples                                                                      []string
}

func (c *harshCounts) bad(format string, a ...any) {
	if c.wrong.Add(1) <= 8 {
		c.mu.Lock()
		c.samples = append(c.samples, fmt.Sprintf(format, a...))
		c.mu.Unlock()
	}
}

func harshSecs(t *testing.T) time.Duration {
	if v, err := strconv.ParseFloat(os.Getenv("HARSH_SECS"), 64); err == nil && v > 0 {
		return time.Duration(v * float64(time.Second))
	}
	if testing.Short() {
		return 500 * time.Millisecond
	}
	return time.Second
}

func runHarshPipeline(t *testing.T, label string, c *Client, dur time.Duration) {
	var cnt harshCounts
	stop := time.Now().Add(dur)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
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
					cnt.commands.Add(3)
					return cmds, keys
				}
				// own checks every Result of the batch: a value must be the
				// command's own; after a failed Exec all must be errors.
				own := func(cmds []*StringCmd, keys []string, execFailed bool) {
					for i, cmd := range cmds {
						v, e := cmd.Result()
						switch {
						case e != nil && !execFailed:
							cnt.bad("%s: error %v after a nil Exec", keys[i], e)
						case e == nil && !isHarshValue(v, keys[i]):
							cnt.bad("%s: wrong value (%d bytes) %.40q", keys[i], len(v), v)
						case e == nil && execFailed:
							// Its own value, but a failed Exec's commands must
							// all report the error.
							cnt.afterFail.Add(1)
						case e == nil:
							cnt.checked.Add(1)
						}
					}
				}
				// Half the batches get a tight deadline.
				timeout := 5 * time.Second
				if rand.IntN(2) == 0 {
					timeout = time.Duration(20+rand.IntN(400)) * time.Microsecond
				}
				cmds, keys := queue(0)
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				err := p.Exec(ctx)
				cancel()
				if err == nil {
					own(cmds, keys, false)
					p.Release()
					continue
				}
				cnt.failed.Add(1)
				own(cmds, keys, true)
				if b%2 == 0 {
					p.Release() // the next batch takes these buffers at once
					continue
				}
				for try := 1; try <= 20; try++ { // retry as it is, no Discard
					cnt.retries.Add(1)
					cmds, keys = queue(try)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					st := time.Now()
					err = p.Exec(ctx)
					cancel()
					if err == nil {
						own(cmds, keys, false)
						break
					}
					own(cmds, keys, true)
					if time.Since(st) > 2*time.Second {
						cnt.hangs.Add(1)
					}
				}
				p.Release()
			}
		}(g)
	}
	wg.Wait()
	t.Logf("%s: batches=%d commands=%d failed=%d retries=%d checked=%d WRONG=%d valueAfterFailedExec=%d hangs=%d panics=%d",
		label, cnt.batches.Load(), cnt.commands.Load(), cnt.failed.Load(), cnt.retries.Load(), cnt.checked.Load(), cnt.wrong.Load(), cnt.afterFail.Load(), cnt.hangs.Load(), cnt.panics.Load())
	for _, s := range cnt.samples {
		t.Logf("%s wrong: %s", label, s)
	}
	if cnt.wrong.Load() > 0 || cnt.afterFail.Load() > 0 || cnt.hangs.Load() > 0 || cnt.panics.Load() > 0 {
		t.Errorf("%s: %d wrong, %d values after a failed Exec, %d hangs, %d panics", label, cnt.wrong.Load(), cnt.afterFail.Load(), cnt.hangs.Load(), cnt.panics.Load())
	}
}

func TestPipelineHarshStandalone(t *testing.T) {
	fake := harshServer(t)
	c, err := NewClient(fake.Addr(), WithPoolSize(16))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	runHarshPipeline(t, "standalone", c, harshSecs(t))
}

func TestPipelineHarshEngineBacked(t *testing.T) {
	opts := harshEngineOptions(t)
	if opts == nil {
		t.Skip("no engine-backed mode on this platform")
	}
	fake := harshServer(t)
	c, err := NewClient(fake.Addr(), append([]Option{WithPoolSize(16)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	runHarshPipeline(t, "engine-backed", c, harshSecs(t))
}
