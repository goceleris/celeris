package redis

import (
	"bufio"
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// A failed Exec must not write the request slab: the connection's reader may
// still be finishing a request of that batch when Exec returns its error (a
// reply that arrives just as the deadline passes). The caller here makes no
// further call on the Pipeline, as on main, where this is race free. Run with
// -race.
func TestPipelineFailedExecDoesNotRaceWithTheReader(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			// Answer around the caller's deadline.
			time.Sleep(time.Duration(200+rand.IntN(400)) * time.Microsecond)
			writeBulk(w, "val-"+cmd[1])
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	deadline := time.Now().Add(20 * time.Second)
	failed := 0
	for i := 0; i < 4000 && time.Now().Before(deadline); i++ {
		p := c.Pipeline()
		p.Get("a")
		p.Get("b")
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(300+rand.IntN(300))*time.Microsecond)
		if err := p.Exec(ctx); err != nil {
			failed++
		}
		cancel()
	}
	t.Logf("%d Execs failed", failed)
}
