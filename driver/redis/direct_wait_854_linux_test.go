//go:build linux

package redis

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// celeris#854, direct-mode pipeline wait. Each test names the mode it runs
// in; the counters are process-wide, so each one takes a snapshot delta and
// none of these tests is parallel.

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestDirectWaitSlowRepliesBlockOncePerExec854: with replies that take 2 and
// 5 ms, auto mode (after the first Exec) and block mode wait in poll(2), about
// once per Exec, and do not hand the wait to netpoll; netpoll mode never
// blocks. The assertions are on counters, which hold on a loaded machine: a
// descheduled client may find the reply already there and skip the block, so
// the lower bound is half the Execs.
func TestDirectWaitSlowRepliesBlockOncePerExec854(t *testing.T) {
	const n = 20
	for _, delay := range []time.Duration{2 * time.Millisecond, 5 * time.Millisecond} {
		for _, m := range []struct {
			name string
			k    directWaitKind
		}{{"auto", directWaitAuto}, {"block", directWaitBlock}, {"netpoll", directWaitNetpoll}} {
			t.Run(m.name+"/"+delay.String(), func(t *testing.T) {
				f := startWaitFake(t)
				f.setDelay(delay)
				cl := newWaitClient(t, f.addr(), 1)
				setDirectWaitForTest(t, m.k)
				execN(t, cl, 4, 10) // warm: arms auto mode
				before := directWaitSnapshot()
				execN(t, cl, n, 10)
				d := directWaitSnapshot().since(before)
				t.Logf("mode=%s delay=%v: %d Execs -> %+v", m.name, delay, n, d)
				if m.k == directWaitNetpoll {
					if d.Blocks != 0 || d.SliceExpiries != 0 || d.Handoffs != 0 {
						t.Errorf("netpoll mode blocked: %+v", d)
					}
					if d.NetpollParks != 0 {
						t.Errorf("netpoll mode counted parks (%d): the netpoll path counts nothing", d.NetpollParks)
					}
					return
				}
				if d.Blocks < n/2 || d.Blocks > 3*n {
					t.Errorf("blocks = %d over %d Execs, want n/2..3n", d.Blocks, n)
				}
				// A starved fake can be 20 ms late (up to 3 of 20 in the loaded
				// runs), so the bound is half the Execs: most waits end in poll(2).
				if d.Handoffs > n/2 {
					t.Errorf("handoffs = %d over %d Execs: a %v reply is well inside the poll budget", d.Handoffs, n, delay)
				}
				if d.NetpollParks != d.Handoffs {
					t.Errorf("netpoll parks = %d, handoffs = %d: the only park is a handoff", d.NetpollParks, d.Handoffs)
				}
			})
		}
	}
}

// TestDirectWaitDefaultBlocksOnSlowReplies854: with nothing pinning the mode,
// a conn whose replies take 2 ms waits in poll(2) when the environment is unset
// and the pool is a measured one (AsyncHandlers, bounded): the default on linux
// is auto, not netpoll. The pools the default was not measured on, the
// WithEngine fallback and an unbounded one (negative PoolSize), stay on netpoll
// when the variable is unset and block when it says auto; "netpoll" never
// blocks.
func TestDirectWaitDefaultBlocksOnSlowReplies854(t *testing.T) {
	const n = 20
	for _, c := range []struct {
		name     string
		env      string // "-" means unset
		pool     int
		fallback bool
		block    bool
	}{
		{"unset/async", "-", 1, false, true},
		{"empty/async", "", 1, false, true},
		{"netpoll/async", "netpoll", 1, false, false},
		{"unset/engine-fallback", "-", 1, true, false},
		{"auto/engine-fallback", "auto", 1, true, true},
		{"unset/unbounded", "-", -1, false, false},
		{"auto/unbounded", "auto", -1, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetDirectWaitForTest(t) // the environment is read by the first Exec below
			t.Setenv(directWaitEnv, c.env)
			if c.env == "-" {
				if err := os.Unsetenv(directWaitEnv); err != nil {
					t.Fatal(err)
				}
			}
			f := startWaitFake(t)
			f.setDelay(2 * time.Millisecond)
			cl := newWaitClientFor(t, f.addr(), c.pool, c.fallback)
			execN(t, cl, 4, 10) // warm: arms auto mode
			before := directWaitSnapshot()
			execN(t, cl, n, 10)
			d := directWaitSnapshot().since(before)
			t.Logf("%s: %d Execs -> %+v", c.name, n, d)
			if c.block && d.Blocks < n/2 {
				t.Errorf("blocks = %d over %d slow Execs, want at least %d", d.Blocks, n, n/2)
			}
			if !c.block && (d.Blocks != 0 || d.Handoffs != 0 || d.NetpollParks != 0) {
				t.Errorf("the netpoll path counted something: %+v", d)
			}
		})
	}
}

// TestDirectWaitSplitReplyValues854: a reply that arrives in chunks of odd
// sizes, with gaps between them (headers and values cut anywhere), parses to
// the right values in every mode, and in block mode the wait was in poll(2).
func TestDirectWaitSplitReplyValues854(t *testing.T) {
	patterns := [][]int{
		{1},
		{3, 5, 997, 2, 4096},
		{4097},
		{7, 1, 1, 1, 64, 1500},
	}
	for _, m := range []struct {
		name string
		k    directWaitKind
	}{{"block", directWaitBlock}, {"auto", directWaitAuto}, {"netpoll", directWaitNetpoll}} {
		for pi, sizes := range patterns {
			t.Run(m.name+"/"+string(rune('a'+pi)), func(t *testing.T) {
				f := startWaitFake(t)
				gap := 300 * time.Microsecond
				if pi == 0 { // 1-byte chunks: keep it short
					sizes = []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 700}
				}
				f.setChunks(gap, sizes...)
				f.setDelay(time.Millisecond)
				cl := newWaitClient(t, f.addr(), 1)
				setDirectWaitForTest(t, m.k)
				before := directWaitSnapshot()
				execN(t, cl, 4, 10)
				d := directWaitSnapshot().since(before)
				if m.k == directWaitBlock && d.Blocks == 0 {
					t.Errorf("block mode never blocked: %+v", d)
				}
				if m.k == directWaitNetpoll && d.Blocks != 0 {
					t.Errorf("netpoll mode blocked: %+v", d)
				}
			})
		}
	}
}

func (f *waitFake) setChunks(gap time.Duration, sizes ...int) {
	f.gap.Store(int64(gap))
	if len(sizes) == 0 {
		f.chunks.Store(nil)
		return
	}
	f.chunks.Store(&sizes)
}

func isDeadlineErr(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout())
}

// TestDirectWaitSilentServerHitsTheDeadline854: a server that never answers
// ends the Exec at the ctx deadline with a deadline error and no hang. In block
// mode the thread polls for the poll budget only (20 slices of 1 ms at most,
// then one handoff), and the goroutine parks in netpoll for the rest. The
// client recovers: the next Exec dials a new conn. The deadline is well past
// the budget so that the handoff is certain; the upper time bound is generous
// (a loaded machine is only asked not to hang).
func TestDirectWaitSilentServerHitsTheDeadline854(t *testing.T) {
	const (
		dline  = 100 * time.Millisecond
		upper  = dline + 250*time.Millisecond
		budget = int(directPollBudget / directPollSlice)
	)
	for _, m := range []struct {
		name string
		k    directWaitKind
	}{{"block", directWaitBlock}, {"netpoll", directWaitNetpoll}} {
		t.Run(m.name, func(t *testing.T) {
			f := startWaitFake(t)
			// Pool of 8: a failed Pipeline.Exec does not hand its conn back (base
			// behaviour, celeris#1268), so a pool of 1 could not serve the
			// recovery Exec below.
			cl := newWaitClient(t, f.addr(), 8)
			setDirectWaitForTest(t, m.k)
			execN(t, cl, 2, 3)
			f.fault.Store(int32(waitFaultSilent))
			before := directWaitSnapshot()
			var err error
			var el time.Duration
			withWatchdog(t, 5*time.Second, func() {
				ctx, cancel := context.WithTimeout(context.Background(), dline)
				defer cancel()
				t0 := time.Now()
				err = waitExec(ctx, cl, 3)
				el = time.Since(t0)
			})
			d := directWaitSnapshot().since(before)
			t.Logf("mode=%s: err=%v after %v, %+v", m.name, err, el, d)
			if err == nil || !isDeadlineErr(err) {
				t.Fatalf("err = %v, want a deadline error", err)
			}
			if el < dline-2*time.Millisecond || el > upper {
				t.Errorf("returned after %v, want %v .. %v", el, dline, upper)
			}
			if m.k == directWaitBlock {
				if d.Blocks != 1 {
					t.Errorf("blocks = %d, want 1 wait episode", d.Blocks)
				}
				if d.Handoffs != 1 {
					t.Errorf("handoffs = %d, want 1: the poll budget runs out before the %v deadline", d.Handoffs, dline)
				}
				if d.SliceExpiries < 1 || d.SliceExpiries > uint64(budget)+1 {
					t.Errorf("slice expiries = %d, want 1..%d: the thread polls for the budget, not the deadline", d.SliceExpiries, budget+1)
				}
				if d.NetpollParks != 1 {
					t.Errorf("netpoll parks = %d, want 1 (the read after the handoff)", d.NetpollParks)
				}
			} else if d.Blocks != 0 || d.Handoffs != 0 {
				t.Errorf("netpoll mode blocked: %+v", d)
			}
			f.fault.Store(int32(waitFaultNone))
			execN(t, cl, 2, 3) // the conn was closed; a new one serves
		})
	}
}

// TestDirectWaitNoDeadlineStallDoesNotHoldTheThread854: with no ctx deadline
// and a server that stalls, the wait in poll(2) ends after the poll budget and
// the goroutine parks in netpoll, as before #854 (celeris#854 review r30, B2).
// The test waits on the handoff counter, not on the clock, then lets the
// server go away: the parked Exec returns an error.
func TestDirectWaitNoDeadlineStallDoesNotHoldTheThread854(t *testing.T) {
	f := startWaitFake(t)
	cl := newWaitClient(t, f.addr(), 8)
	setDirectWaitForTest(t, directWaitBlock)
	execN(t, cl, 2, 3)
	f.fault.Store(int32(waitFaultSilent))
	before := directWaitSnapshot()
	done := make(chan error, 1)
	go func() { done <- waitExec(context.Background(), cl, 3) }()
	limit := time.After(10 * time.Second)
	// The handoff is counted before the park, so wait for both: a snapshot
	// between the two would see Handoffs == 1 and NetpollParks == 0.
	for d := directWaitSnapshot().since(before); d.Handoffs == 0 || d.NetpollParks == 0; d = directWaitSnapshot().since(before) {
		select {
		case err := <-done:
			t.Fatalf("Exec returned (%v) while the server was silent", err)
		case <-limit:
			t.Fatalf("no handoff within 10 s: %+v", directWaitSnapshot().since(before))
		case <-time.After(time.Millisecond):
		}
	}
	d := directWaitSnapshot().since(before)
	t.Logf("after the handoff: %+v", d)
	if d.Blocks != 1 || d.Handoffs != 1 || d.NetpollParks != 1 {
		t.Errorf("want one block, one handoff, one park: %+v", d)
	}
	if d.SliceExpiries < 1 || d.SliceExpiries > uint64(directPollBudget/directPollSlice)+1 {
		t.Errorf("slice expiries = %d: the poll must stop at the budget", d.SliceExpiries)
	}
	f.dropConns() // the server goes away: the parked read fails
	select {
	case err := <-done:
		if err == nil {
			t.Error("Exec succeeded against a silent server")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked Exec did not return after the server closed")
	}
	if got := directWaitSnapshot().since(before); got.Blocks != 1 || got.Handoffs != 1 {
		t.Errorf("the wait polled again after the handoff: %+v", got)
	}
}

// TestDirectWaitFINAndRSTMidReply854: a server that closes with a FIN or an
// RST before or in the middle of a reply fails the Exec quickly, without a
// hang and without a spin: few waits, few slice expiries, little CPU.
func TestDirectWaitFINAndRSTMidReply854(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault waitFault
		gap   time.Duration
	}{
		{"fin-mid", waitFaultFINMid, 0},
		{"fin-mid-gap", waitFaultFINMid, 2 * time.Millisecond},
		{"rst-mid", waitFaultRSTMid, 0},
		{"rst-mid-gap", waitFaultRSTMid, 2 * time.Millisecond},
		{"fin-before-reply", waitFaultFINPre, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startWaitFake(t)
			f.setDelay(2 * time.Millisecond)
			cl := newWaitClient(t, f.addr(), 8)
			setDirectWaitForTest(t, directWaitBlock)
			execN(t, cl, 2, 10)
			f.fault.Store(int32(tc.fault))
			if tc.gap > 0 {
				f.setChunks(tc.gap, 4096)
			}
			before := directWaitSnapshot()
			cpu0 := cpuTime()
			var err error
			var el time.Duration
			withWatchdog(t, 5*time.Second, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				t0 := time.Now()
				err = waitExec(ctx, cl, 10)
				el = time.Since(t0)
			})
			cpu := cpuTime() - cpu0
			d := directWaitSnapshot().since(before)
			t.Logf("%s: err=%v after %v, cpu %v, %+v", tc.name, err, el, cpu, d)
			if err == nil {
				t.Fatal("Exec succeeded on a truncated reply")
			}
			if el > time.Second {
				t.Errorf("took %v to fail", el)
			}
			if d.Blocks < 1 {
				t.Errorf("blocks = %d, want the wait in poll(2)", d.Blocks)
			}
			// A slice lasts 1 ms, so a thread that waits expires at most once per
			// ms of elapsed time; a spin would expire far more often.
			if d.Blocks > 6 || d.SliceExpiries > uint64(el/time.Millisecond)+4 {
				t.Errorf("spin: %+v in %v", d, el)
			}
			if cpu > 150*time.Millisecond {
				t.Errorf("cpu = %v for one failed Exec", cpu)
			}
			f.fault.Store(int32(waitFaultNone))
			f.setChunks(0)
			execN(t, cl, 2, 10)
		})
	}
}

// TestDirectWaitConcurrent854: many goroutines share a small pool on two Ps,
// each Exec checked value by value, in every mode. Run under -race.
func TestDirectWaitConcurrent854(t *testing.T) {
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	for _, m := range []struct {
		name string
		k    directWaitKind
	}{{"auto", directWaitAuto}, {"block", directWaitBlock}, {"netpoll", directWaitNetpoll}} {
		t.Run(m.name, func(t *testing.T) {
			f := startWaitFake(t)
			f.setDelay(time.Millisecond)
			f.setChunks(100*time.Microsecond, 5000)
			cl := newWaitClient(t, f.addr(), 4)
			setDirectWaitForTest(t, m.k)
			before := directWaitSnapshot()
			var wg sync.WaitGroup
			withWatchdog(t, 60*time.Second, func() {
				for g := 0; g < 8; g++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := 0; i < 25; i++ {
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							err := waitExec(ctx, cl, 10)
							cancel()
							if err != nil {
								t.Errorf("exec: %v", err)
								return
							}
						}
					}()
				}
				wg.Wait()
			})
			d := directWaitSnapshot().since(before)
			t.Logf("mode=%s: 200 Execs -> %+v", m.name, d)
			if m.k != directWaitNetpoll && d.Blocks == 0 {
				t.Errorf("mode %s never blocked: %+v", m.name, d)
			}
			if m.k == directWaitNetpoll && d.Blocks != 0 {
				t.Errorf("netpoll mode blocked: %+v", d)
			}
		})
	}
}
