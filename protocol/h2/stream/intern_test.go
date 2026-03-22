package stream

import (
	"testing"
	"unsafe"
)

func TestInternH2HeaderName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Pseudo-headers
		{":method", ":method"},
		{":path", ":path"},
		{":scheme", ":scheme"},
		{":authority", ":authority"},
		{":status", ":status"},

		// Common headers
		{"accept", "accept"},
		{"accept-encoding", "accept-encoding"},
		{"accept-language", "accept-language"},
		{"authorization", "authorization"},
		{"cache-control", "cache-control"},
		{"connection", "connection"},
		{"content-encoding", "content-encoding"},
		{"content-length", "content-length"},
		{"content-type", "content-type"},
		{"cookie", "cookie"},
		{"date", "date"},
		{"etag", "etag"},
		{"expect", "expect"},
		{"host", "host"},
		{"if-modified-since", "if-modified-since"},
		{"if-none-match", "if-none-match"},
		{"location", "location"},
		{"origin", "origin"},
		{"referer", "referer"},
		{"server", "server"},
		{"set-cookie", "set-cookie"},
		{"transfer-encoding", "transfer-encoding"},
		{"upgrade", "upgrade"},
		{"user-agent", "user-agent"},
		{"vary", "vary"},
		{"x-forwarded-for", "x-forwarded-for"},
		{"x-real-ip", "x-real-ip"},
		{"x-request-id", "x-request-id"},

		// Unknown headers (passthrough)
		{"unknown-header", "unknown-header"},
		{"x-custom", "x-custom"},
		{"", ""},
		{"x", "x"},
	}
	for _, tt := range tests {
		got := internH2HeaderName(tt.input)
		if got != tt.want {
			t.Errorf("internH2HeaderName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestInternH2HeaderNameReturnsStaticString(t *testing.T) {
	// Build a string from a fresh allocation so it cannot share backing
	// data with the static var. internH2HeaderName should return the
	// static constant, allowing the HPACK-allocated string to be GC'd.
	name := string([]byte(":method"))
	interned := internH2HeaderName(name)
	if interned != ":method" {
		t.Fatalf("expected %q, got %q", ":method", interned)
	}
	// The interned string should point to the static var, not the input.
	if unsafe.StringData(interned) == unsafe.StringData(name) {
		t.Error("interned string should not share backing data with input")
	}
}

func TestInternH2HeaderNamePassthroughUnknown(t *testing.T) {
	unknown := "x-totally-custom-header"
	got := internH2HeaderName(unknown)
	if got != unknown {
		t.Errorf("expected passthrough for unknown header, got %q", got)
	}
	// Passthrough should return the exact same string (same pointer).
	if unsafe.StringData(got) != unsafe.StringData(unknown) {
		t.Error("passthrough should return the exact input string")
	}
}

func BenchmarkInternH2HeaderName(b *testing.B) {
	names := []string{":method", ":path", "content-type", "accept-encoding", "x-custom-header"}
	b.ResetTimer()
	for range b.N {
		for _, n := range names {
			_ = internH2HeaderName(n)
		}
	}
}
