//go:build linux

package memcached

// The harsh probe for the request-reuse race: no instrument, no hook. Large
// values (24 KiB: a reply spans reads, so the worker is mid-reply when the
// caller's deadline ends), tight deadlines on half the calls (20-420 us), and an
// immediate reuse of the Client by 8 goroutines, every value and key returned
// checked against the key it was asked for. On the event-loop path (a Client
// opened WithEngine of a server that has no loop of its own, on the driver's
// standalone loop) the unfixed driver returned another key's value, a key
// GetMulti had not asked for, or a data race report under -race, at a rate of
// about 1 call in 100,000. It is the acceptance test of releaseRequest; the
// delay-instrument test (late_reply_reuse_linux_test.go) is the sharper one.
//
// Short mode (the default) runs 20,000 calls per case and takes a few seconds;
// it must pass on the fixed tree and need not fail on the unfixed one. Long mode
// is -harsh.calls=N (or CELERIS_MC_HARSH_CALLS=N): N calls per case, which is
// how the failure before the fix and the pass after it are shown (5,000,000).
//
// Cases: the event-loop path ("loop") and the default direct path ("direct", the
// control: it reads on the caller's goroutine and has no late writer), over the
// in-process fake (text) and, when CELERIS_MEMCACHED_ADDR is set, a real server
// (text and binary).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
)

var harshCalls = flag.Int64("harsh.calls", 0, "calls per case of TestHarshLateReplyReuse (0: short mode, or CELERIS_MC_HARSH_CALLS)")

func harshCallBudget() int64 {
	if *harshCalls > 0 {
		return *harshCalls
	}
	if s := os.Getenv("CELERIS_MC_HARSH_CALLS"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 20_000
}

type harshResult struct {
	calls, failed, okChecks, wrong, longFailed int64
	samples                                    []string
}

// path is how the Client reaches the server: "loop" (WithEngine of a server that
// has not been started: the driver's standalone loop, where a worker goroutine
// can run onRecv after the caller has returned), "std" (the same loop, through
// WithEngine of a STARTED std server that has no loop of its own, with four
// goroutines of HTTP load on it: the CPU pressure of a Client in a server's
// process) or "direct" (no engine: the caller's own goroutine reads).
func runHarsh(t *testing.T, addr string, proto Protocol, path string, budget int64) harshResult {
	t.Helper()
	// WithTimeout: the per-operation limit (default 3 s) must not end a call before its own
	// 20 s context does, or the long-deadline check below would count a slow CI host.
	opts := []Option{WithProtocol(proto), WithDialTimeout(5 * time.Second), WithTimeout(20 * time.Second)}
	switch path {
	case "loop":
		opts = append(opts, WithEngine(looptest.Unstarted()))
	case "std":
		srv := looptest.StartStd(t)
		opts = append(opts, WithEngine(srv))
		stopLoad := httpLoad(srv.Addr().String(), 4)
		defer stopLoad()
	}
	c, err := NewClient(addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	const nkeys = 500
	ctx := context.Background()
	prefix := fmt.Sprintf("hsh%s", strconv.FormatInt(time.Now().UnixNano(), 36))
	key := func(i int) string { return fmt.Sprintf("%s-k%d", prefix, i) }
	pad := strings.Repeat("x", 24000)
	val := func(k string) string { return "v-" + k + pad }
	for i := 0; i < nkeys; i++ {
		if err := c.Set(ctx, key(i), val(key(i)), 0); err != nil {
			t.Fatal(err)
		}
	}

	var calls, failed, okChecks, wrong, longFailed atomic.Int64
	var mu sync.Mutex
	var samples []string
	bad := func(s string) {
		if wrong.Add(1) <= 12 {
			mu.Lock()
			samples = append(samples, strings.ReplaceAll(s, prefix, "P"))
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; calls.Add(1) <= budget; i++ {
				tiny := rand.IntN(2) == 0
				d := 20 * time.Second
				if tiny {
					d = time.Duration(20+rand.IntN(400)) * time.Microsecond
				}
				cctx, cancel := context.WithTimeout(ctx, d)
				kind := rand.IntN(6) // 2 multiget, 1 gets, 3 get
				if kind < 2 {
					ks := []string{key(rand.IntN(nkeys)), key(rand.IntN(nkeys)), key(rand.IntN(nkeys))}
					m, err := c.GetMulti(cctx, ks...)
					cancel()
					if err != nil {
						failed.Add(1)
						if !tiny {
							longFailed.Add(1) // a 20 s deadline (and a 20 s per-operation limit) never ends a command on a healthy server
						}
						continue
					}
					want := map[string]bool{ks[0]: true, ks[1]: true, ks[2]: true}
					for k, v := range m {
						switch {
						case !want[k]:
							bad(fmt.Sprintf("GetMulti(%v) returned the foreign key %s=%.30q", ks, k, v))
						case v != val(k):
							bad(fmt.Sprintf("GetMulti %s=%.30q", k, v))
						default:
							okChecks.Add(1)
						}
					}
					for k := range want {
						if _, ok := m[k]; !ok {
							bad(fmt.Sprintf("GetMulti(%v) is missing %s", ks, k))
						}
					}
					continue
				}
				k := key(rand.IntN(nkeys))
				if kind == 2 {
					it, err := c.Gets(cctx, k)
					cancel()
					switch {
					case err != nil && !errors.Is(err, ErrCacheMiss):
						failed.Add(1)
						if !tiny {
							longFailed.Add(1)
						}
					case err != nil:
						bad(fmt.Sprintf("Gets(%s) is a miss: the key exists", k))
					case string(it.Value) != val(k) || (it.Key != "" && it.Key != k):
						bad(fmt.Sprintf("Gets(%s)=key %q %.30q", k, it.Key, it.Value))
					default:
						okChecks.Add(1)
					}
					continue
				}
				v, err := c.Get(cctx, k)
				cancel()
				switch {
				case err != nil && !errors.Is(err, ErrCacheMiss):
					failed.Add(1)
					if !tiny {
						longFailed.Add(1)
					}
				case err != nil:
					bad(fmt.Sprintf("Get(%s) is a miss: the key exists", k))
				case v != val(k):
					bad(fmt.Sprintf("Get(%s)=%.30q", k, v))
				default:
					okChecks.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	return harshResult{min(calls.Load(), budget), failed.Load(), okChecks.Load(), wrong.Load(), longFailed.Load(), samples}
}

func TestHarshLateReplyReuse(t *testing.T) {
	type target struct {
		name  string
		addr  string
		proto Protocol
	}
	budget := harshCallBudget()
	targets := []target{{"fake/text", startFake(t).Addr(), ProtocolText}}
	if realAddr := strings.TrimSpace(os.Getenv("CELERIS_MEMCACHED_ADDR")); realAddr != "" {
		targets = append(targets, target{"real/text", realAddr, ProtocolText}, target{"real/binary", realAddr, ProtocolBinary})
	}
	for _, tg := range targets {
		for _, path := range []string{"loop", "std", "direct"} {
			t.Run(tg.name+"/"+path, func(t *testing.T) {
				start := time.Now()
				r := runHarsh(t, tg.addr, tg.proto, path, budget)
				t.Logf("HARSH %s/%s calls=%d failed=%d longFailed=%d okChecks=%d WRONG=%d (%.1fs)",
					tg.name, path, r.calls, r.failed, r.longFailed, r.okChecks, r.wrong, time.Since(start).Seconds())
				for _, s := range r.samples {
					t.Log("  wrong: " + s)
				}
				if r.wrong != 0 {
					t.Fatalf("%d replies reached a command that did not ask for them", r.wrong)
				}
				if r.longFailed != 0 {
					t.Fatalf("%d commands with a 20 s deadline failed on a healthy server", r.longFailed)
				}
			})
		}
	}
}

// httpLoad keeps n goroutines sending requests to a started server at addr until
// the returned stop function is called.
func httpLoad(addr string, n int) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: n}}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if resp, err := cl.Get("http://" + addr + "/"); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}
		}()
	}
	return func() { close(done); wg.Wait(); cl.CloseIdleConnections() }
}
