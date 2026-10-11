package std

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// celeris#855: StreamDetached is what Context.Detach calls on std, once when a
// stream starts. It must be safe on a writer that cannot set deadlines (a
// recorder, a wrapper that hides the connection), where there is nothing
// armed to clear.
func TestStreamDetachedOnAWriterWithoutDeadlines855(t *testing.T) {
	rw := &stdResponseWriter{w: httptest.NewRecorder()}
	rw.StreamDetached() // must not panic or write
	if rw.flushed {
		t.Fatal("StreamDetached wrote a response")
	}
}

// BenchmarkStreamDetached855 is the cost of the call itself on a live
// connection (ResponseController, then both deadlines), repeated inside one
// request: what a stream pays once, when it starts. Requests that do not
// detach never reach it.
func BenchmarkStreamDetached855(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rw := &stdResponseWriter{w: w}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			rw.StreamDetached()
		}
		b.StopTimer()
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		b.Fatal(err)
	}
	_ = resp.Body.Close()
}
