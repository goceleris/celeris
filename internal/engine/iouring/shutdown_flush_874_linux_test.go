//go:build linux

package iouring

import (
	"reflect"
	"strings"
	"testing"
)

// celeris#874, io_uring half: the counters the loop batches per iteration
// (reqBatch, bytesReadBatch, bytesWrittenBatch, ringBytesBatch,
// closeFDDeferredBatch, linkArmBatch) move into the engine-wide atomics near
// the top of the next iteration, and the loop returns into shutdown from
// several places before that: a close queued late in the last iteration was
// never counted in Metrics().CloseFDDeferred, the number celeris#793 quotes as
// exact, and the bytes and requests of that iteration went the same way.

// batchFields874 are the Worker's uint64 fields named *Batch: the batches.
func batchFields874(t *testing.T) []string {
	t.Helper()
	var out []string
	rt := reflect.TypeOf(Worker{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() == reflect.Uint64 && strings.HasSuffix(f.Name, "Batch") {
			out = append(out, f.Name)
		}
	}
	if len(out) < 6 {
		t.Fatalf("found only %v: the guard no longer sees the batches it exists for", out)
	}
	return out
}

// TestShutdownFlushesEveryBatch874 gives every batch a distinct value on a
// worker wired to an engine's counters, shuts the worker down, and reads the
// totals back through Engine.Metrics(). A batch the shutdown does not flush
// stays non-zero on the worker, which the loop over the *Batch fields (found
// by reflection, so a batch added later is covered, not listed) catches
// without the test knowing its counter.
func TestShutdownFlushesEveryBatch874(t *testing.T) {
	e := &Engine{}
	w := &Worker{
		listenFD:     -1,
		reqCount:     &e.metrics.reqCount,
		bytesRead:    &e.metrics.bytesRead,
		bytesWritten: &e.metrics.bytesWritten,
		recvArm:      &e.metrics.recvArm,
		zc:           &e.metrics.zc,
		handoffLoss:  &e.metrics.handoffLoss,
	}
	fields := batchFields874(t)
	rv := reflect.ValueOf(w).Elem()
	for i, name := range fields {
		rv.FieldByName(name).SetUint(uint64(1) << (i + 1))
	}
	// Distinct values, so a counter fed from the wrong batch is not a pass.
	w.reqBatch, w.bytesReadBatch, w.bytesWrittenBatch = 3, 5, 7
	w.ringBytesBatch, w.closeFDDeferredBatch, w.linkArmBatch = 11, 13, 17
	for _, name := range fields {
		if rv.FieldByName(name).Uint() == 0 {
			t.Fatalf("setup: %s is zero", name)
		}
	}
	if m := e.Metrics(); m.RequestCount|m.BytesRead|m.BytesWritten|m.RingBytes|m.CloseFDDeferred|m.RecvLinkedArms != 0 {
		t.Fatalf("setup: the engine's counters are not zero: %+v", m)
	}

	w.shutdown()

	m := e.Metrics()
	t.Logf("celeris874 after shutdown: RequestCount=%d BytesRead=%d BytesWritten=%d RingBytes=%d CloseFDDeferred=%d RecvLinkedArms=%d",
		m.RequestCount, m.BytesRead, m.BytesWritten, m.RingBytes, m.CloseFDDeferred, m.RecvLinkedArms)
	for _, c := range []struct {
		name      string
		got, want uint64
	}{
		{"RequestCount", m.RequestCount, 3},
		{"BytesRead", m.BytesRead, 5},
		{"BytesWritten", m.BytesWritten, 7},
		{"RingBytes", m.RingBytes, 11},
		{"CloseFDDeferred", m.CloseFDDeferred, 13},
		{"RecvLinkedArms", m.RecvLinkedArms, 17},
	} {
		if c.got != c.want {
			t.Errorf("Metrics().%s = %d after shutdown, want %d: the batch was not flushed", c.name, c.got, c.want)
		}
	}
	for _, name := range fields {
		if left := rv.FieldByName(name).Uint(); left != 0 {
			t.Errorf("Worker.%s = %d after shutdown: a batch shutdown does not flush (celeris#874)", name, left)
		}
	}
}

// TestShutdownFlushesABareWorker874: shutdown reaches the flush on a Worker
// built by hand, whose shared counters are not wired: it must drop the batch,
// not fault.
func TestShutdownFlushesABareWorker874(t *testing.T) {
	w := &Worker{listenFD: -1}
	w.reqBatch, w.bytesReadBatch, w.bytesWrittenBatch = 1, 1, 1
	w.ringBytesBatch, w.closeFDDeferredBatch, w.linkArmBatch = 1, 1, 1
	w.shutdown()
	if w.reqBatch|w.bytesReadBatch|w.bytesWrittenBatch|w.ringBytesBatch|w.closeFDDeferredBatch|w.linkArmBatch != 0 {
		t.Fatal("a batch survived shutdown on a bare Worker")
	}
}
