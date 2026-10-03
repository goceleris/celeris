package sse

import "testing"

// TestOwnEventCopiesEveryString pins ownEvent (celeris#732): every string of
// the returned Event keeps its value after the bytes it was built from
// change, each field gets its own bytes back (empty fields included), and
// Retry is kept.
func TestOwnEventCopiesEveryString(t *testing.T) {
	cases := []struct{ id, event, data string }{
		{"id-1", "kind-a", "msg-a"},
		{"", "kind-b", "msg-b"},
		{"id-3", "", "msg-c"},
		{"id-4", "kind-d", ""},
		{"", "", "msg-e"},
		{"", "", ""},
	}
	for _, tc := range cases {
		buf := []byte(tc.id + tc.event + tc.data)
		i, j := len(tc.id), len(tc.id)+len(tc.event)
		e := Event{ID: view(buf[:i]), Event: view(buf[i:j]), Data: view(buf[j:]), Retry: 7}
		got := ownEvent(e)
		for k := range buf {
			buf[k] = 'z'
		}
		if got.ID != tc.id || got.Event != tc.event || got.Data != tc.data || got.Retry != 7 {
			t.Errorf("ownEvent(%q, %q, %q) after the bytes changed: ID %q Event %q Data %q Retry %d",
				tc.id, tc.event, tc.data, got.ID, got.Event, got.Data, got.Retry)
		}
	}
}
