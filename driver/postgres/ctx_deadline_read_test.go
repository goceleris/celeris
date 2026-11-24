package postgres

import (
	"context"
	"testing"
	"time"
)

// What a statement pays by kind of context: nothing is registered with the
// context, so a context without a deadline allocates exactly what Background does.
func TestAContextWithoutADeadlineAllocatesNothingExtra(t *testing.T) {
	f := startSilentPG(t, 0)
	p := openDirectPool(t, f.addr)
	run := func(ctx context.Context) func() {
		return func() {
			if _, err := p.ExecContext(ctx, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
		}
	}
	type kv struct{}
	run(context.Background())() // dial, warm the pools
	bg := testing.AllocsPerRun(200, run(context.Background()))
	val := testing.AllocsPerRun(200, run(context.WithValue(context.Background(), kv{}, 1)))
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnly := testing.AllocsPerRun(200, run(cctx))
	// the process-wide count includes the fake backend, hence the 1 of slack
	if val > bg+1 || cancelOnly > bg+1 {
		t.Fatalf("allocs per statement: background %v, with a value %v, cancel-only %v: a context without a deadline must cost the same", bg, val, cancelOnly)
	}
}

// A statement under a deadline sets the socket's read deadline and clears it
// when it ends: the next statement, under no deadline, must not time out on the
// previous one's deadline.
func TestTheReadDeadlineOfAStatementIsClearedWhenItEnds(t *testing.T) {
	f := startSilentPG(t, 0)
	p := openDirectPool(t, f.addr)
	for i := range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		if _, err := p.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		cancel()
		time.Sleep(60 * time.Millisecond) // past the previous statement's deadline
		if _, err := p.ExecContext(context.Background(), "SELECT 2"); err != nil {
			t.Fatalf("iteration %d: the next statement hit the previous one's deadline: %v", i, err)
		}
	}
}

// A deadline that passes just as the statement finishes, in both orders, leaves
// the conn usable.
func TestADeadlineRacingTheEndOfTheStatementLeavesTheConnUsable(t *testing.T) {
	f := startSilentPG(t, 0)
	p := openDirectPool(t, f.addr)
	for i := range 300 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%7)*100*time.Microsecond)
		_, _ = p.ExecContext(ctx, "SELECT 1") // may or may not be cut by the deadline
		cancel()
		if _, err := p.ExecContext(context.Background(), "SELECT 2"); err != nil {
			t.Fatalf("iteration %d: the next statement failed: %v", i, err)
		}
	}
}
