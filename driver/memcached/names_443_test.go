package memcached

import (
	"errors"
	"fmt"
	"testing"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// The engine sentinels the event loop returns are reachable as
// memcached.Err* (celeris#443): a caller can match them with errors.Is
// without importing the engine package.
func TestEngineSentinelsNamed443(t *testing.T) {
	if ErrQueueFull != engine.ErrQueueFull || ErrUnknownFD != engine.ErrUnknownFD {
		t.Fatal("ErrQueueFull/ErrUnknownFD are not the engine's sentinels")
	}
	if !errors.Is(fmt.Errorf("write: %w", engine.ErrQueueFull), ErrQueueFull) {
		t.Fatal("a wrapped engine.ErrQueueFull does not match ErrQueueFull")
	}

	// A real event loop: Write on a descriptor the worker never registered.
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	err = prov.WorkerLoop(0).Write(1<<30, []byte("version\r\n"))
	if !errors.Is(err, ErrUnknownFD) {
		t.Fatalf("Write on an unregistered fd: err = %v, want ErrUnknownFD", err)
	}
	if errors.Is(err, ErrQueueFull) {
		t.Fatalf("Write on an unregistered fd: err = %v matches ErrQueueFull too", err)
	}
	if len(PoolStats{PerWorker: []PoolWorkerStats{{Idle: 1}}}.PerWorker) != 1 {
		t.Fatal("PoolStats")
	}
}
