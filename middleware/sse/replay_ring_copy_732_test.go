package sse

import (
	"context"
	"testing"
	"unsafe"
)

// view returns a string that shares b's bytes, as a request string on epoll
// and io_uring shares the connection's receive buffer.
func view(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

// TestRingBufferAppendKeepsACopy pins the ring replay store's copy
// (celeris#732): an event appended with strings that share a buffer replays
// with its own values after the buffer changes, under the ring's ID.
func TestRingBufferAppendKeepsACopy(t *testing.T) {
	r := NewRingBuffer(4)
	buf := []byte("caller-idkind-amsg-a")
	id, err := r.Append(context.Background(), Event{ID: view(buf[:9]), Event: view(buf[9:15]), Data: view(buf[15:])})
	if err != nil {
		t.Fatal(err)
	}
	for k := range buf {
		buf[k] = 'z'
	}
	evs, err := r.Since(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != id || id != "1" || evs[0].Event != "kind-a" || evs[0].Data != "msg-a" {
		t.Fatalf("replayed %+v (Append returned ID %q), want one event ID 1, event kind-a, data msg-a", evs, id)
	}
}
