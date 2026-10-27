package conn

import (
	"context"
	"testing"
)

// BenchmarkProcessH1Backlogged890 is the per-request path of ProcessH1 with the
// engines' back-pressure query installed (H1State.WriteBacklogged, answering
// false: the request is served), the path that celeris#890 item 22's hook sits
// beside. The hook is called only when the query answers true, so this is the
// figure a change to that branch must leave alone.
func BenchmarkProcessH1Backlogged890(b *testing.B) {
	st := NewH1State()
	st.WriteBacklogged = func() bool { return false }
	w := func([]byte) {}
	ctx := context.Background()
	req := []byte("GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	b.SetBytes(int64(len(req)))
	b.ReportAllocs()
	for b.Loop() {
		if err := ProcessH1(ctx, req, st, nopHandler1029{}, w); err != nil {
			b.Fatal(err)
		}
	}
}

// TestBacklogRefusedHookFiresOnlyOnRefusal890: the hook is told the peer address
// when a request finds the connection backlogged, and is not called for a
// request that is served.
func TestBacklogRefusedHookFiresOnlyOnRefusal890(t *testing.T) {
	var got []string
	f := func(remote string) { got = append(got, remote) }
	BacklogRefusedHook.Store(&f)
	t.Cleanup(func() { BacklogRefusedHook.Store(nil) })

	st := NewH1State()
	st.RemoteAddr = "192.0.2.7:4242"
	backlogged := false
	st.WriteBacklogged = func() bool { return backlogged }
	w := func([]byte) {}
	req := []byte("GET /x HTTP/1.1\r\nHost: a\r\n\r\n")

	if err := ProcessH1(context.Background(), req, st, nopHandler1029{}, w); err != nil {
		t.Fatalf("a request that is served: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the hook fired %v for a request that was served", got)
	}
	backlogged = true
	if err := ProcessH1(context.Background(), req, st, nopHandler1029{}, w); err != ErrWriteBacklog {
		t.Fatalf("a backlogged request: %v, want ErrWriteBacklog", err)
	}
	if len(got) != 1 || got[0] != "192.0.2.7:4242" {
		t.Fatalf("the hook fired %v, want exactly the peer address once", got)
	}
}
