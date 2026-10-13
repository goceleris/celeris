//go:build linux

package celeris

import (
	"strings"
	"testing"
)

// pinObs592 builds the observation of a promoted route (settled arm) or of an
// all-async control with `samples` post-promotion /ping samples, in which the
// probe was found stalled (100 ms outstanding) `captures` times and the
// event-loop goroutines were parked on a mutex at `parked` of those. The rig
// itself (TestAdaptiveSettledRouteRetime592/iouring/...) measures the numbers;
// the tables pin the verdict on numbers it measured. captures < 0 builds an
// observation with no stack tally at all.
func pinObs592(engine string, mode stall589Mode, samples, captures, parked int) stall589Obs {
	o := stall589Obs{
		engine: engine, mode: mode.String(), kvConns: 8,
		promotedInBound: true, promotedAfter: true, asyncPromotedConns: 8,
		samples:        samples,
		refSamples:     0, // no reference window: the epoll ratio is skipped, so only the pin assertion is under test
		kvSamples:      0, // likewise for the controls' ratio
		queuedTimeFrac: 0.5,
	}
	if captures >= 0 {
		o.stacks = &stackTally589{captures: captures, parked: parked, loops: 2 * captures} // two workers, as in every capture measured
	}
	return o
}

// noLoops592 is an observation whose stack captures found no event-loop
// goroutine at all, as if isLoopGoroutine589 no longer matched the engine.
func noLoops592(o stall589Obs) stall589Obs {
	o.stacks.loops = 0
	return o
}

// TestSettledRetimedIOUringBar592 pins the io_uring settled arm's judgement
// (celeris#886 item 1) at the level of the assertion, with no engine.
//
// The observable is the pin itself, not a latency. Every latency statistic of
// the post-promotion /ping samples was measured on the fixed tree and on the
// celeris#593 TryLock reverted, idle and under celeris#628's load (--cpus 4, 28
// busy loops, two cold-cache builds) and under twice that (--cpus 2, 56 busy
// loops, three builds): the fraction of samples over D/2 is 0.114-0.270 pinned
// against 0.000-0.158 free, the share of the window spent in them 0.506-0.903
// against 0.000-0.448, and a starved free worker is as slow as a pinned one for
// as long as the box starves it. What a free worker never does is wait on a
// mutex: in 370 stalled-probe stack captures of the fixed tree (90 arms, idle and
// under both loads) not one event-loop goroutine was parked on one, and the
// pinned worker is parked on one at 0.625-1.000 of the captures.
func TestSettledRetimedIOUringBar592(t *testing.T) {
	cases := []struct {
		name    string
		o       stall589Obs
		wantErr string // "" = the arm passes
	}{
		{"io_uring, the fixed tree, idle (no stalled probe, so no capture)", pinObs592("iouring", stall589Settled, 950, 0, 0), ""},
		{"io_uring, the fixed tree, starved (89 samples, 11 captures, none parked)", pinObs592("iouring", stall589Settled, 89, 11, 0), ""},
		{"io_uring, one stray capture on a mutex is below the floor", pinObs592("iouring", stall589Settled, 89, 11, 1), ""},
		{"io_uring, the celeris#593 TryLock reverted (33 captures, 30 parked)", pinObs592("iouring", stall589Settled, 135, 33, 30), "celeris#593"},
		{"io_uring, the celeris#593 TryLock reverted, at the floor (3 of 3)", pinObs592("iouring", stall589Settled, 100, 3, 3), "celeris#593"},
		{"io_uring, the celeris#593 TryLock reverted, at a half of the captures (10 of 20)", pinObs592("iouring", stall589Settled, 100, 20, 10), "celeris#593"},
		{"io_uring, parked at a small minority of the captures is not the pin (3 of 20)", pinObs592("iouring", stall589Settled, 100, 20, 3), ""},
		{"io_uring, the celeris#593 TryLock reverted, the lowest share of 75 arms (10 of 16)", pinObs592("iouring", stall589Settled, 71, 16, 10), "celeris#593"},
		{"io_uring, at a quarter of the captures (5 of 20)", pinObs592("iouring", stall589Settled, 100, 20, 5), "celeris#593"},
		{"io_uring, just under a quarter of the captures (4 of 20)", pinObs592("iouring", stall589Settled, 100, 20, 4), ""},
		{"io_uring, no post-promotion samples at all is not a pass", pinObs592("iouring", stall589Settled, 0, 0, 0), "samples"},
		{"io_uring, too few samples to judge is not a pass", pinObs592("iouring", stall589Settled, 5, 0, 0), "samples"},
		{"io_uring, no stack tally at all is not a pass", pinObs592("iouring", stall589Settled, 950, -1, 0), "stack"},
		{"io_uring, stacks taken but none held an event-loop goroutine is not a pass", noLoops592(pinObs592("iouring", stall589Settled, 100, 5, 0)), "event-loop goroutine"},
		{"epoll with the same stacks is judged on its ratio, not on this", pinObs592("epoll", stall589Settled, 150, 33, 30), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertSettledRetimed592(tc.o)
			if (err != nil) != (tc.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("assertSettledRetimed592 = %v, want an error containing %q (\"\" = none)", err, tc.wantErr)
			}
		})
	}
}

// TestCtlWorkerFreeIOUringBar592: the control arms (every /kv conn
// async-dispatched, so the worker must be free) apply the same observable on
// io_uring. With the celeris#593 TryLock reverted both controls passed the
// median ratio while their worker was parked on the mutex.
func TestCtlWorkerFreeIOUringBar592(t *testing.T) {
	for _, tc := range []struct {
		name    string
		o       stall589Obs
		wantErr string
	}{
		{"io_uring, the fixed tree, idle", pinObs592("iouring", stall589ExplicitAsync, 950, 0, 0), ""},
		{"io_uring, the fixed tree, starved (4 captures, none parked)", pinObs592("iouring", stall589Learning, 82, 4, 0), ""},
		{"io_uring, the celeris#593 TryLock reverted (33 captures, 28 parked)", pinObs592("iouring", stall589Learning, 158, 33, 28), "celeris#593"},
		{"io_uring, the celeris#593 TryLock reverted, async control (33 captures, 31 parked)", pinObs592("iouring", stall589ExplicitAsync, 133, 33, 31), "celeris#593"},
		{"io_uring, too few samples to judge is not a pass", pinObs592("iouring", stall589ExplicitAsync, 5, 0, 0), "samples"},
		{"io_uring, no stack tally at all is not a pass", pinObs592("iouring", stall589ExplicitAsync, 950, -1, 0), "stack"},
		{"io_uring, stacks taken but none held an event-loop goroutine is not a pass", noLoops592(pinObs592("iouring", stall589ExplicitAsync, 100, 5, 0)), "event-loop goroutine"},
		{"epoll with the same stacks is judged on its ratio", pinObs592("epoll", stall589ExplicitAsync, 150, 33, 30), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := assertCtlWorkerFree628(tc.o)
			if (err != nil) != (tc.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("assertCtlWorkerFree628 = %v, want an error containing %q (\"\" = none)", err, tc.wantErr)
			}
		})
	}
}

// TestClassifyLoopStack589 pins the classifier the io_uring arms' pin
// observable stands on, on the goroutine dumps it reads. Without this a rename
// of checkTimeouts, or a Go release that prints a parked mutex differently,
// would turn the assertion into one that can never fire.
func TestClassifyLoopStack589(t *testing.T) {
	// Verbatim from a run (golang:1.27.1, linux/arm64; camp/logs/m-quiet.log and
	// fixed-c628x-stack.log): the first goroutine found parked on a mutex with the
	// celeris#593 TryLock reverted, and the first one found in the kernel wait.
	const pinned = `goroutine 77 [sync.Mutex.Lock, locked to thread]:
internal/sync.runtime_SemacquireMutex(0x76401763daa8?, 0x54?, 0x1aa?)
	/usr/local/go/src/runtime/sema.go:95 +0x28
internal/sync.(*Mutex).lockSlow(0x764017dce178)
	/usr/local/go/src/internal/sync/mutex.go:149 +0x174
internal/sync.(*Mutex).Lock(...)
	/usr/local/go/src/internal/sync/mutex.go:70
sync.(*Mutex).Lock(...)
	/usr/local/go/src/sync/mutex.go:46
github.com/goceleris/celeris/internal/engine/iouring.snapshotH1Deadlines(0x764017dd8b08)
	/src/internal/engine/iouring/worker.go:6852 +0x90
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).checkTimeouts(0x7640175f2608)
	/src/internal/engine/iouring/worker.go:6999 +0x3a4
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).run(0x7640175f2608, {0xdd3928, 0x764017628870})
	/src/internal/engine/iouring/worker.go:1756 +0x1688
github.com/goceleris/celeris/internal/engine/iouring.(*Engine).runWatched(0x0?, {0xdd3928?, 0x764017628870?}, 0x0?)
	/src/internal/engine/iouring/worker_exit_848_linux.go:55 +0x58
github.com/goceleris/celeris/internal/engine/iouring.(*Engine).Listen.func2()
	/src/internal/engine/iouring/engine.go:421 +0x4c
created by github.com/goceleris/celeris/internal/engine/iouring.(*Engine).Listen in goroutine 73
	/src/internal/engine/iouring/engine.go:419 +0x820`
	const idle = `goroutine 34 [syscall, locked to thread]:
syscall.Syscall6(0x1aa, 0x7, 0x0, 0x1, 0x9, 0x36a96019b28, 0x18)
	/usr/local/go/src/syscall/syscall_linux.go:96 +0x2c
github.com/goceleris/celeris/internal/engine/iouring.(*Ring).SubmitAndWaitTimeout(0x36a960165a0, 0xffff61ef6230?)
	/src/internal/engine/iouring/ring.go:358 +0x94
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).run(0x36a96012008, {0xdd1500, 0x36a95f24050})
	/src/internal/engine/iouring/worker.go:1498 +0xdc8
github.com/goceleris/celeris/internal/engine/iouring.(*Engine).runWatched(0x0?, {0xdd1500?, 0x36a95f24050?}, 0x0?)
	/src/internal/engine/iouring/worker_exit_848_linux.go:55 +0x58
github.com/goceleris/celeris/internal/engine/iouring.(*Engine).Listen.func2()
	/src/internal/engine/iouring/engine.go:421 +0x4c
created by github.com/goceleris/celeris/internal/engine/iouring.(*Engine).Listen in goroutine 9
	/src/internal/engine/iouring/engine.go:419 +0x820`
	for _, g := range []string{pinned, idle} {
		if !isLoopGoroutine589(g) {
			t.Fatalf("isLoopGoroutine589 does not recognise an io_uring worker goroutine:\n%s", g)
		}
	}
	if isLoopGoroutine589("goroutine 7 [sleep]:\ntime.Sleep(0x1)\n\t/usr/local/go/src/runtime/time.go:368 +0x150\ngithub.com/goceleris/celeris.(*gatedKV).Set(0x1)") {
		t.Fatal("isLoopGoroutine589 recognises a handler goroutine as an event loop")
	}
	for _, tc := range []struct {
		name, stack, want string
		parked            bool
	}{
		{"parked in checkTimeouts on detachMu", pinned, "checkTimeouts_detachMu_Lock", true},
		{"idle in the kernel wait", idle, "kernel_wait", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLoopStack589(tc.stack)
			if got != tc.want || mutexParked589(got) != tc.parked {
				t.Fatalf("classifyLoopStack589 = %q (parked %v), want %q (parked %v)", got, mutexParked589(got), tc.want, tc.parked)
			}
		})
	}
}
