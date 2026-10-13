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
		o.stacks = &stackTally589{captures: captures, parked: parked}
	}
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
// mutex: in 140 stalled-probe stack captures of the fixed tree under the harsh
// load not one event-loop goroutine was parked on one, and the pinned worker
// is parked on one at most captures.
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
		{"io_uring, the celeris#593 TryLock reverted, at half the captures (10 of 20)", pinObs592("iouring", stall589Settled, 100, 20, 10), "celeris#593"},
		{"io_uring, parked at a minority of the captures is not the pin (3 of 20)", pinObs592("iouring", stall589Settled, 100, 20, 3), ""},
		{"io_uring, no post-promotion samples at all is not a pass", pinObs592("iouring", stall589Settled, 0, 0, 0), "samples"},
		{"io_uring, too few samples to judge is not a pass", pinObs592("iouring", stall589Settled, 5, 0, 0), "samples"},
		{"io_uring, no stack tally at all is not a pass", pinObs592("iouring", stall589Settled, 950, -1, 0), "stack"},
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
	const pinned = `goroutine 41 [sync.Mutex.Lock, 212 minutes]:
sync.runtime_SemacquireMutex(0x0?, 0x0?, 0x0?)
	runtime/sema.go:95 +0x28
sync.(*Mutex).lockSlow(0x4000123456)
	sync/mutex.go:149 +0x168
sync.(*Mutex).Lock(...)
	sync/mutex.go:70
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).snapshotH1Deadlines(0x400011c000, 0x4000234000)
	internal/engine/iouring/worker.go:6852 +0x70
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).checkTimeouts(0x400011c000)
	internal/engine/iouring/worker.go:6790 +0x94
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).run(0x400011c000)
	internal/engine/iouring/worker.go:1100 +0x2f4`
	const idle = `goroutine 42 [syscall]:
syscall.Syscall6(0x1aa, 0x5, 0x0, 0x1, 0x1, 0x0, 0x0)
	syscall/syscall_linux.go:96 +0x20
github.com/goceleris/celeris/internal/engine/iouring.(*Ring).SubmitAndWait(0x4000200000, 0x1)
	internal/engine/iouring/ring.go:300 +0x48
github.com/goceleris/celeris/internal/engine/iouring.(*Worker).run(0x400011d000)
	internal/engine/iouring/worker.go:1100 +0x2f4`
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
