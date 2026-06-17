package stream

import "testing"

// TestResetH1StreamClearsUniqueFields locks in the contract that ResetH1Stream
// owns: the fields the per-request caller does NOT unconditionally overwrite.
// rawBody is the load-bearing one — a bodyless GET that follows a request with
// a body must not inherit the stale body (celeris#346).
func TestResetH1StreamClearsUniqueFields(t *testing.T) {
	s := NewH1Stream(1)

	s.SetRawBody([]byte("previous request body"))
	s.GetBuf().WriteString("buffered data")
	s.lazyHeadersBuilt = true
	s.pseudoMaterialized = true
	s.headersSent.Store(true)

	ResetH1Stream(s)

	if s.rawBody != nil {
		t.Fatalf("rawBody = %q, want nil (stale body would leak into next request)", s.rawBody)
	}
	if s.Data != nil {
		t.Fatalf("Data = %v, want nil (buffer must be returned to the pool)", s.Data)
	}
	if s.lazyHeadersBuilt {
		t.Fatal("lazyHeadersBuilt = true, want false")
	}
	if s.pseudoMaterialized {
		t.Fatal("pseudoMaterialized = true, want false")
	}
	if s.headersSent.Load() {
		t.Fatal("headersSent = true, want false")
	}
}

func BenchmarkResetH1Stream(b *testing.B) {
	s := NewH1Stream(1)
	body := []byte("hello")
	b.ReportAllocs()
	for b.Loop() {
		s.SetRawBody(body)
		s.lazyHeadersBuilt = true
		s.pseudoMaterialized = true
		s.headersSent.Store(true)
		ResetH1Stream(s)
	}
}
