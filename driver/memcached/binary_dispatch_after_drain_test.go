package memcached

// dispatchBinary takes the head of the bridge with Head, not Pop, and used to ignore the result of its
// own Pop: when the conn is closed from another goroutine in between (Client.Close drains it), the
// drain has already taken the request, failed it and finished it, and the worker went on to write the
// reply into it (binStatus, resultErr on a miss) and to finish it again: a data race with the drain's
// own writes, and a request whose error was overwritten. The pause of the event loop package holds the
// worker between Head and Pop, as a descheduled worker would be.

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// missReply is the binary reply of a get of a missing key (status 1) with opaque op.
func missReply(op uint32) []byte {
	body := []byte("Not found")
	h := make([]byte, 24)
	h[0], h[1] = 0x81, 0x00
	binary.BigEndian.PutUint16(h[6:], 1) // KEY_NOT_FOUND
	binary.BigEndian.PutUint32(h[8:], uint32(len(body)))
	binary.BigEndian.PutUint32(h[12:], op)
	return append(h, body...)
}

func TestBinaryReplyAfterTheDrainDoesNotTouchTheRequest(t *testing.T) {
	s := newMCState(true)
	req := getRequest(context.Background())
	req.kind = kindBinary
	req.opaque = 7
	s.bridge.Enqueue(req)

	pause := eventloop.NewPause(1, 200*time.Millisecond)
	t.Cleanup(eventloop.SetDispatchPause(pause))

	done := make(chan error, 1)
	go func() { done <- s.processRecv(missReply(7)) }() // the worker: takes the head, then is held
	deadline := time.Now().Add(5 * time.Second)
	for pause.Hits() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker never reached its dispatch")
		}
		time.Sleep(time.Millisecond)
	}
	drainErr := errors.New("the conn was closed")
	s.drainWithError(drainErr) // Client.Close on another goroutine: takes, fails and finishes the request

	select {
	case err := <-done:
		if err == nil {
			t.Error("the dispatch of a reply whose request the drain had taken returned nil, want the head mismatch error (as the text END path does)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not return")
	}
	if req.resultErr != drainErr {
		t.Errorf("the request's error is %v, want the drain's %v: the worker wrote into a request the drain had taken", req.resultErr, drainErr)
	}
	if req.binStatus != 0 {
		t.Errorf("binStatus = %d: the worker wrote its reply into a request the drain had taken", req.binStatus)
	}
}
