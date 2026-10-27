package sse

import (
	"bytes"
	"testing"
	"time"
)

// TestSlowClientBlockQueuesTheCopy888 (celeris#888 item 47): under
// ClientPolicyBlock the Send that finds the queue full waits on the queue with
// the copy of the event (made at the top of sendQueued), not with the caller's
// strings, which are views of a buffer that changes while it waits. The
// OnSlowClient callback opens the gate itself, after it has changed the
// caller's bytes, so the blocked Send is released only then; it does not fit
// the dropFullQueue harness's own release, which this one replaces.
// TestSlowClientCallbackKeepsItsEvent888 covers drop and close; the block arm
// had no ownership test. The wire must carry the original strings.
func TestSlowClientBlockQueuesTheCopy888(t *testing.T) {
	ctx, gs := newGatedContext(t)
	buf := []byte("42update-kindpayload-bytes")
	cfg := Config{
		MaxQueueDepth: 2,
		OnSlowClient: func(*Client, Event) ClientPolicy {
			go func() {
				time.Sleep(30 * time.Millisecond)
				for k := range buf {
					buf[k] = 'z'
				}
				gs.release() // the blocked Send and the drain go on only now
			}()
			return ClientPolicyBlock
		},
	}
	dropFullQueue(t, ctx, gs, cfg, func(client *Client) {
		if err := client.Send(Event{ID: view(buf[:2]), Event: view(buf[2:13]), Data: view(buf[13:]), Retry: 9}); err != nil {
			t.Errorf("Send under ClientPolicyBlock: %v", err)
		}
	})
	gs.mu.Lock()
	wire := bytes.Join(gs.chunks, nil)
	gs.mu.Unlock()
	want := "id: 42\nevent: update-kind\nretry: 9\ndata: payload-bytes\n\n"
	if !bytes.Contains(wire, []byte(want)) {
		t.Errorf("the wire does not carry the event as it was sent:\n got %q\nwant it to contain %q", wire, want)
	}
}
