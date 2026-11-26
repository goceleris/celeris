package postgres

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// A write refused because the loop's write backlog is over its cap (ErrQueueFull)
// sent nothing and left the connection alive: it is not a lost connection, so it
// does not match ErrClosed, and the conn stays usable.

type fullLoop struct{ engine.WorkerLoop }

func (fullLoop) Write(int, []byte) error { return engine.ErrQueueFull }

func TestWriteQueueFullIsNotConnectionLoss(t *testing.T) {
	c := &pgConn{loop: fullLoop{}}
	err := c.writeRaw([]byte("x"))
	if !errors.Is(err, engine.ErrQueueFull) {
		t.Fatalf("writeRaw = %v, want ErrQueueFull", err)
	}
	if errors.Is(err, ErrClosed) {
		t.Fatalf("writeRaw = %v: matches ErrClosed, but the connection is alive", err)
	}
	if c.closed.Load() {
		t.Fatal("the conn was marked closed")
	}
	// The sync poll's caller: the request failed with ErrQueueFull.
	req := acquirePgRequest()
	req.fail(engine.ErrQueueFull)
	if err := req.failedErr(engine.ErrQueueFull); !errors.Is(err, engine.ErrQueueFull) || errors.Is(err, ErrClosed) {
		t.Fatalf("failedErr = %v, want ErrQueueFull and not ErrClosed", err)
	}
	releasePgRequest(req)
}
