package celeris

import (
	"net/http"
	"testing"
)

func TestAdapt(t *testing.T) {
	stdHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from stdlib"))
	})

	adapted := Adapt(stdHandler)

	s, rw := newTestStream("GET", "/bridge")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{adapted}
	_ = c.Next()

	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "hello from stdlib" {
		t.Fatalf("expected 'hello from stdlib', got '%s'", string(rw.body))
	}
}

func TestAdaptFunc(t *testing.T) {
	adapted := AdaptFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("func"))
	})

	s, rw := newTestStream("GET", "/funcbridge")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{adapted}
	_ = c.Next()

	if string(rw.body) != "func" {
		t.Fatalf("expected 'func', got '%s'", string(rw.body))
	}
}

func TestAdaptWithHeaders(t *testing.T) {
	stdHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "test" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	adapted := Adapt(stdHandler)

	s, rw := newTestStream("GET", "/bridge-headers")
	s.Headers = append(s.Headers, [2]string{"x-custom", "test"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{adapted}
	_ = c.Next()

	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
}

func TestAdaptContentLengthValidation(t *testing.T) {
	stdHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Negative or invalid content-length should not crash.
		if r.ContentLength < 0 {
			t.Fatal("content-length should not be negative")
		}
		w.WriteHeader(http.StatusOK)
	})

	adapted := Adapt(stdHandler)

	// Test with negative content-length.
	s, rw := newTestStream("POST", "/cl-neg")
	s.Headers = append(s.Headers, [2]string{"content-length", "-1"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{adapted}
	_ = c.Next()

	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
}

func TestAdaptContentLengthInvalid(t *testing.T) {
	stdHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Invalid content-length should result in 0 (default).
		if r.ContentLength != 0 {
			t.Fatalf("expected 0 for invalid CL, got %d", r.ContentLength)
		}
		w.WriteHeader(http.StatusOK)
	})

	adapted := Adapt(stdHandler)

	s, rw := newTestStream("POST", "/cl-invalid")
	s.Headers = append(s.Headers, [2]string{"content-length", "notanumber"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{adapted}
	_ = c.Next()

	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
}

func TestBridgeResponseBufferCap(t *testing.T) {
	w := &bridgeResponseWriter{}
	w.code = http.StatusOK

	// Write data below the cap — should succeed.
	data := make([]byte, 1024)
	_, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the cap constant is 100MB.
	if maxBridgeResponseBytes != 100<<20 {
		t.Fatalf("expected 100MB cap, got %d", maxBridgeResponseBytes)
	}
}

func TestBridgeResponseOverflow(t *testing.T) {
	w := &bridgeResponseWriter{}
	w.code = http.StatusOK

	// Write exactly maxBridgeResponseBytes to fill the buffer.
	chunk := make([]byte, 1<<20) // 1 MB
	for range 100 {
		_, err := w.Write(chunk)
		if err != nil {
			t.Fatalf("unexpected error at fill: %v", err)
		}
	}

	// Buffer is now at 100MB. One more byte should fail.
	_, err := w.Write([]byte{0x01})
	if err == nil {
		t.Fatal("expected error when exceeding maxBridgeResponseBytes")
	}
	if err != errBridgeResponseTooLarge {
		t.Fatalf("expected errBridgeResponseTooLarge, got %v", err)
	}
}
