package memcached

import (
	"context"
	"math"
	"testing"
	"time"
)

// The per-operation limit costs a command no allocation: a client with Timeout
// (the 3 s default or a set one) allocates exactly what one with the limit off does,
// on the direct and the loop paths, with a context that cannot be cancelled. (A
// context built per command cost 4 allocations and broke the strict budgets of
// TestAllocBudgets; the CI step that runs them skips -race; the comparison below
// holds under -race too, both sides being inflated alike.)
//
// A context that CAN be cancelled (WithCancel, WithTimeout, an HTTP/2 stream's)
// costs a direct conn context.AfterFunc's two allocations, which arm the wake of
// its blocked read (the loop paths select on the context and cost nothing). That
// price is budgeted in TestAllocBudgets (GET_small_cancellable, which CI runs
// without -race): AllocsPerRun is too noisy under -race to hold it here.
func TestOperationLimitDoesNotAllocate1218(t *testing.T) {
	f := startFake(t)
	f.store.kv["k"] = memItem{value: []byte("v"), cas: 1}
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			// Both clients are built and warmed first, then measured in turns, so
			// whatever the process does meanwhile reaches both alike.
			get := func(timeout time.Duration) func() {
				c := cl.new(t, Config{Addr: f.Addr(), Timeout: timeout})
				bg := context.Background()
				if _, err := c.Get(bg, "k"); err != nil { // warm the pool
					t.Fatal(err)
				}
				return func() {
					if _, err := c.Get(bg, "k"); err != nil {
						t.Fatal(err)
					}
				}
			}
			withLimit, without := get(3*time.Second), get(-1)
			// Under -race sync.Pool drops Puts at random, so a round of either side
			// can come out an allocation above its true cost (about 1 round in 35
			// under -race on 2 CPUs, either side alike), and with one round per side
			// the noise of one was set against the other's. The noise only adds
			// allocations: the least of the rounds of a side is its cost. A limit
			// that allocates raises every round of its side, so its least too.
			const rounds = 7
			onMin, offMin := math.Inf(1), math.Inf(1)
			for range rounds {
				onMin = min(onMin, testing.AllocsPerRun(100, withLimit))
				offMin = min(offMin, testing.AllocsPerRun(100, without))
			}
			t.Logf("Get: %.1f allocs/op with the limit, %.1f without (least of %d rounds)", onMin, offMin, rounds)
			if onMin > offMin {
				t.Errorf("the per-operation limit costs %.1f allocations per command, want 0", onMin-offMin)
			}
		})
	}
}
