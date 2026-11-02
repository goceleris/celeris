package memcached

import (
	"context"
	"testing"
	"time"
)

// The per-operation limit costs a command no allocation: a client with Timeout
// (the 3 s default or a set one) allocates exactly what one with the limit off does,
// on the direct and the loop paths. (A context built per command cost 4 allocations
// and broke the strict budgets of TestAllocBudgets; the CI step that runs them
// skips -race; the comparison below holds under -race too, both sides being inflated alike.)
func TestOperationLimitDoesNotAllocate1218(t *testing.T) {
	f := startFake(t)
	f.store.kv["k"] = memItem{value: []byte("v"), cas: 1}
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			ctx := context.Background()
			allocs := func(timeout time.Duration) float64 {
				c := cl.new(t, Config{Addr: f.Addr(), Timeout: timeout})
				if _, err := c.Get(ctx, "k"); err != nil { // warm the pool
					t.Fatal(err)
				}
				return testing.AllocsPerRun(200, func() {
					if _, err := c.Get(ctx, "k"); err != nil {
						t.Fatal(err)
					}
				})
			}
			on, off := allocs(3*time.Second), allocs(-1)
			t.Logf("Get: %.1f allocs/op with the limit, %.1f without", on, off)
			if on > off {
				t.Errorf("the per-operation limit costs %.1f allocations per command, want 0", on-off)
			}
		})
	}
}
