//go:build redis

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
)

// Stress of the context handling of a direct-mode conn against a real server, on ONE
// shared pool per mode: workers mix commands whose contexts end after 0..2 ms (a
// deadline or a cancel()) with innocent commands on contexts that cannot end or that
// end in 10 s, over single commands, pipelines, MULTI/EXEC, a 256 KiB reply (several
// reads) and the blocking commands (BLPOP, BRPOP, BZPOPMIN, XREAD BLOCK 0) that only
// the context can end. A deadline ends every kind of command; a cancel() ends the
// blocking ones (an ordinary command under a context with no deadline is not watched
// and runs to the server's answer). Every reply is checked against its own command:
// a reply that belongs to an abandoned command (a stray) shows as a wrong value, for
// the commands that completed under an expiring context as well as for the innocent
// ones. A call that ends in anything but a success or the context's own error, a call
// that does not return, and a pool that is not healthy at the end fail the test.
//
// Every command that a context ends costs a conn (the abandoned reply is on its
// way, so the conn is closed and the pool dials another): the commands that end
// are paced (CELERIS_STRESS_EXPIRY_RATE per second over all workers, default 250)
// so that the client's TIME_WAIT sockets do not exhaust the ephemeral ports of a
// laptop; the innocent commands are not.
//
// The modes: a Client with no engine (the driver's own loop), on an engine that
// runs handlers asynchronously (direct), on an engine that has not started (the
// loop on Linux, direct elsewhere), and on a started epoll and a started io_uring
// engine (Linux; direct, "integrated"), each over RESP3 and RESP2.
//
// CELERIS_STRESS_SECONDS sets the duration of each mode (default 3).
func TestCtxStressSharedPool(t *testing.T) {
	addr := addrFromEnv(t)
	secs := 3
	if v, err := strconv.Atoi(os.Getenv("CELERIS_STRESS_SECONDS")); err == nil && v > 0 {
		secs = v
	}
	if testing.Short() {
		secs = 1
	}
	rate := 250
	if v, err := strconv.Atoi(os.Getenv("CELERIS_STRESS_EXPIRY_RATE")); err == nil && v > 0 {
		rate = v
	}
	for name, opts := range stressModes(t) {
		for _, proto := range []string{"RESP3", "RESP2"} {
			t.Run(name+"/"+proto, func(t *testing.T) {
				o := append(clientOpts(), celredis.WithPoolSize(4), celredis.WithDialTimeout(2*time.Second))
				o = append(o, opts...)
				if proto == "RESP2" {
					o = append(o, celredis.WithForceRESP2())
				}
				stressMode(t, addr, o, time.Duration(secs)*time.Second, rate)
			})
		}
	}
}

type stressCounts struct {
	ok, enforced, innocent, wrong, other, dialRaw atomic.Int64
}

func stressMode(t *testing.T, addr string, opts []celredis.Option, dur time.Duration, rate int) {
	cl, err := celredis.NewClient(addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = cl.Close()
		}
	}()
	base := runtime.NumGoroutine()
	prefix := fmt.Sprintf("ctxstress:%d:", time.Now().UnixNano())
	const nkeys = 8
	big := strings.Repeat("0123456789abcdef", 16<<10) // 256 KiB
	ctx := context.Background()
	keyVal := func(i int) (string, string) { return prefix + "k" + strconv.Itoa(i), "value-of-key-" + strconv.Itoa(i) }
	for i := 0; i < nkeys; i++ {
		k, v := keyVal(i)
		if err := cl.Set(ctx, k, v, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	bigKey, emptyKey := prefix+"big", prefix+"empty"
	if err := cl.Set(ctx, bigKey, big, time.Minute); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = cl.Del(context.Background(), bigKey, emptyKey) }()

	var st stressCounts
	const workers = 12
	var lastOp [workers]atomic.Int64 // start of the call in progress, 0 if none
	stop := make(chan struct{})
	// permits paces the commands that a context ends.
	permits := make(chan struct{}, 1)
	go func() {
		tk := time.NewTicker(time.Second / time.Duration(rate))
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				select {
				case permits <- struct{}{}:
				default:
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w)+1, uint64(time.Now().UnixNano())))
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				lastOp[w].Store(time.Now().UnixNano())
				stressIteration(t, cl, rng, &st, permits, stop, keyVal, nkeys, bigKey, big, emptyKey, fmt.Sprintf("%s%d-%d", prefix, w, n))
				lastOp[w].Store(0)
			}
		}()
	}
	// Watchdog: a call in progress for 10 s is a hang.
	end := time.Now().Add(dur)
	hung := false
	for time.Now().Before(end) && !hung {
		time.Sleep(100 * time.Millisecond)
		for w := range lastOp {
			if s := lastOp[w].Load(); s != 0 && time.Since(time.Unix(0, s)) > 10*time.Second {
				hung = true
			}
		}
	}
	close(stop)
	fin := make(chan struct{})
	go func() { wg.Wait(); close(fin) }()
	select {
	case <-fin:
	case <-time.After(15 * time.Second):
		hung = true
	}
	if hung {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("a call did not return in 10 s (hang); goroutines:\n%s", buf)
	}
	t.Logf("ok=%d enforced=%d innocent-ok=%d wrong(stray)=%d other-errors=%d dial-expiry-raw=%d", st.ok.Load(), st.enforced.Load(), st.innocent.Load(), st.wrong.Load(), st.other.Load(), st.dialRaw.Load())
	if st.wrong.Load() != 0 || st.other.Load() != 0 {
		t.Fatalf("%d replies of another command, %d other errors", st.wrong.Load(), st.other.Load())
	}
	if st.enforced.Load() == 0 {
		t.Fatal("no command was ended by its context: the stress exercised nothing")
	}

	// Healthy at the end: the same pool serves every worker's commands.
	var hw sync.WaitGroup
	var bad atomic.Int64
	for w := 0; w < 8; w++ {
		hw.Add(1)
		go func() {
			defer hw.Done()
			for i := 0; i < 50; i++ {
				nonce := fmt.Sprintf("final-%d-%d", w, i)
				v, err := cl.DoString(ctx, "ECHO", nonce)
				if err != nil || v != nonce {
					bad.Add(1)
				}
			}
		}()
	}
	hw.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d of 400 commands after the stress failed or got another reply", bad.Load())
	}
	if err := cl.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := cl.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	// No goroutine left behind by the watchers.
	var n int
	for i := 0; i < 50; i++ {
		if n = runtime.NumGoroutine(); n <= base {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n > base+2 {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutines: %d before the client, %d after Close:\n%s", base, n, buf)
	}
}

func stressCtx(rng *rand.Rand, expiring bool) (context.Context, context.CancelFunc) {
	if !expiring {
		switch rng.IntN(3) {
		case 0:
			return context.Background(), func() {}
		case 1:
			return context.WithTimeout(context.Background(), 10*time.Second)
		default:
			return context.WithCancel(context.Background())
		}
	}
	d := time.Duration(rng.IntN(2001)) * time.Microsecond
	if rng.IntN(2) == 0 {
		return context.WithTimeout(context.Background(), d)
	}
	c, cancel := context.WithCancel(context.Background())
	if d == 0 {
		cancel()
		return c, cancel
	}
	timer := time.AfterFunc(d, cancel)
	return c, func() { timer.Stop(); cancel() }
}

func stressIteration(t *testing.T, cl *celredis.Client, rng *rand.Rand, st *stressCounts, permits <-chan struct{}, stop <-chan struct{}, keyVal func(int) (string, string), nkeys int, bigKey, big, emptyKey, nonce string) {
	expiring := rng.IntN(10) >= 4
	op := rng.IntN(7)
	if expiring || op >= 5 { // op 5 and 6 are always ended by a context
		select {
		case <-permits:
		case <-stop:
			return
		}
	}
	ctx, cancel := stressCtx(rng, expiring)
	defer cancel()
	var err error
	var wrong string
	switch op {
	case 0: // ECHO
		var v string
		v, err = cl.DoString(ctx, "ECHO", nonce)
		if err == nil && v != nonce {
			wrong = fmt.Sprintf("ECHO %q -> %q", nonce, v)
		}
	case 1: // GET
		i := rng.IntN(nkeys)
		k, want := keyVal(i)
		var v string
		v, err = cl.Get(ctx, k)
		if err == nil && v != want {
			wrong = fmt.Sprintf("GET %s -> %q", k, v)
		}
	case 2: // big reply, several reads
		var v string
		v, err = cl.Get(ctx, bigKey)
		if err == nil && v != big {
			wrong = fmt.Sprintf("GET big -> %d bytes", len(v))
		}
	case 3: // pipeline
		p := cl.Pipeline()
		idx := []int{rng.IntN(nkeys), rng.IntN(nkeys), rng.IntN(nkeys)}
		cmds := make([]*celredis.StringCmd, len(idx))
		for j, i := range idx {
			k, _ := keyVal(i)
			cmds[j] = p.Get(k)
		}
		err = p.Exec(ctx)
		if err == nil {
			for j, i := range idx {
				_, want := keyVal(i)
				if v, e := cmds[j].Result(); e != nil || v != want {
					wrong = fmt.Sprintf("pipeline[%d] -> %q, %v", j, v, e)
				}
			}
		}
	case 4: // MULTI/EXEC
		var tx *celredis.Tx
		tx, err = cl.TxPipeline(context.Background())
		if err != nil {
			st.other.Add(1)
			t.Errorf("TxPipeline: %v", err)
			return
		}
		idx := []int{rng.IntN(nkeys), rng.IntN(nkeys)}
		cmds := make([]*celredis.StringCmd, len(idx))
		for j, i := range idx {
			k, _ := keyVal(i)
			cmds[j] = tx.Get(k)
		}
		err = tx.Exec(ctx)
		_ = tx.Discard()
		if err == nil {
			for j, i := range idx {
				_, want := keyVal(i)
				if v, e := cmds[j].Result(); e != nil || v != want {
					wrong = fmt.Sprintf("tx[%d] -> %q, %v", j, v, e)
				}
			}
		}
	case 5: // blocking, never answered: only the context ends it
		if !expiring {
			ctx2, cancel2 := context.WithTimeout(context.Background(), time.Duration(1+rng.IntN(15))*time.Millisecond)
			defer cancel2()
			ctx = ctx2
			expiring = true
		}
		switch rng.IntN(4) {
		case 0:
			_, err = cl.Do(ctx, "BLPOP", emptyKey, "0")
		case 1:
			_, err = cl.Do(ctx, "BRPOP", emptyKey, "0")
		case 2:
			_, err = cl.Do(ctx, "BZPOPMIN", emptyKey, "0")
		default:
			_, err = cl.Do(ctx, "XREAD", "BLOCK", "0", "STREAMS", emptyKey, "$")
		}
		if err == nil {
			wrong = "BLPOP on an empty key returned"
		}
	default: // blocking with a server timeout that is longer than the context
		if !expiring {
			ctx2, cancel2 := context.WithCancel(context.Background())
			time.AfterFunc(time.Duration(1+rng.IntN(15))*time.Millisecond, cancel2)
			defer cancel2()
			ctx = ctx2
			expiring = true
		}
		_, err = cl.Do(ctx, "BLPOP", emptyKey, "1")
		if err == nil {
			wrong = "BLPOP on an empty key returned"
		}
	}
	switch {
	case wrong != "":
		st.wrong.Add(1)
		t.Errorf("a reply of another command: %s", wrong)
	case err == nil:
		st.ok.Add(1)
		if !expiring {
			st.innocent.Add(1)
		}
	case expiring && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		st.enforced.Add(1)
	case expiring && isDialTimeout(err):
		// A context that ends while the pool dials a conn: the standard dialer reports
		// the socket's "i/o timeout", which does not match context.DeadlineExceeded.
		// The dial is not the wait this fix is about (it is the same on v1.5.11 and
		// in loop mode); counted apart.
		st.dialRaw.Add(1)
	case expiring && isNilBLPOP(err):
		st.ok.Add(1) // BLPOP "1": the server's own timeout came first, a nil reply
	default:
		st.other.Add(1)
		t.Errorf("unexpected error (expiring=%v, op %d, ctx err %v): %T %v", expiring, op, ctx.Err(), err, err)
	}
}

func isNilBLPOP(err error) bool { return errors.Is(err, celredis.ErrNil) }

func isDialTimeout(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial" && errors.Is(err, os.ErrDeadlineExceeded)
}

// stressModes is every way a Client can run, by name.
func stressModes(t *testing.T) map[string][]celredis.Option {
	m := map[string][]celredis.Option{
		"standalone":                nil,
		"engine-async-std (direct)": {celredis.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std, AsyncHandlers: true}))},
	}
	for k, v := range stressPlatformModes(t) {
		m[k] = v
	}
	return m
}
