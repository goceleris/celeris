package frame

import (
	"testing"
)

func TestHeaderEncoderDecodeRoundTrip(t *testing.T) {
	encoder := NewHeaderEncoder()
	defer encoder.Close()

	headers := [][2]string{
		{":method", "GET"},
		{":path", "/"},
		{":scheme", "https"},
		{":authority", "example.com"},
		{"content-type", "text/html"},
	}

	encoded, err := encoder.Encode(headers)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("Encoded block is empty")
	}

	decoder := NewHeaderDecoder(4096)
	decoded, err := decoder.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded) != len(headers) {
		t.Fatalf("Decoded %d headers, want %d", len(decoded), len(headers))
	}

	for i, h := range decoded {
		if h[0] != headers[i][0] || h[1] != headers[i][1] {
			t.Errorf("Header %d: got %v, want %v", i, h, headers[i])
		}
	}
}

func TestHeaderEncoderPseudoHeaders(t *testing.T) {
	encoder := NewHeaderEncoder()
	defer encoder.Close()

	headers := [][2]string{
		{":status", "200"},
		{"content-length", "0"},
	}

	encoded, err := encoder.Encode(headers)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decoder := NewHeaderDecoder(4096)
	decoded, err := decoder.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded) != 2 {
		t.Fatalf("Expected 2 decoded headers, got %d", len(decoded))
	}
	if decoded[0][0] != ":status" || decoded[0][1] != "200" {
		t.Errorf("First header: got %v, want [:status, 200]", decoded[0])
	}
}

func TestHeaderEncoderLargeBlock(t *testing.T) {
	encoder := NewHeaderEncoder()
	defer encoder.Close()

	headers := make([][2]string, 0, 50)
	for i := range 50 {
		headers = append(headers, [2]string{
			"x-custom-header-" + string(rune('a'+i%26)),
			"value-" + string(rune('0'+i%10)),
		})
	}

	encoded, err := encoder.Encode(headers)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decoder := NewHeaderDecoder(65536)
	decoded, err := decoder.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded) != len(headers) {
		t.Errorf("Decoded %d headers, want %d", len(decoded), len(headers))
	}
}

func TestEncodeBorrow(t *testing.T) {
	encoder := NewHeaderEncoder()
	defer encoder.Close()

	headers := [][2]string{
		{":method", "POST"},
		{":path", "/api"},
	}

	borrowed, err := encoder.EncodeBorrow(headers)
	if err != nil {
		t.Fatalf("EncodeBorrow: %v", err)
	}
	if len(borrowed) == 0 {
		t.Fatal("EncodeBorrow returned empty slice")
	}

	// Decode the borrowed bytes to verify correctness
	decoder := NewHeaderDecoder(4096)
	// Copy because borrowed slice is backed by encoder buffer
	data := make([]byte, len(borrowed))
	copy(data, borrowed)
	decoded, err := decoder.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("Decoded %d headers, want 2", len(decoded))
	}
}

func TestHeaderEncoderClosePoolReturn(t *testing.T) {
	encoder := NewHeaderEncoder()

	// Encode something first
	_, err := encoder.Encode([][2]string{{":method", "GET"}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Close should return buffer to pool
	encoder.Close()

	// Double close should be safe
	encoder.Close()
}

func TestHeaderDecoderMaxSize(t *testing.T) {
	encoder := NewHeaderEncoder()
	defer encoder.Close()

	// Encode headers with a value that fits within a small decoder
	headers := [][2]string{
		{":method", "GET"},
	}
	encoded, err := encoder.Encode(headers)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Small max size decoder should still decode standard HPACK-encoded content
	decoder := NewHeaderDecoder(256)
	decoded, err := decoder.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("Expected 1 header, got %d", len(decoded))
	}
}

func TestHeaderEncoderMultipleEncodes(t *testing.T) {
	encoder := NewHeaderEncoder()
	defer encoder.Close()

	for i := range 5 {
		headers := [][2]string{
			{":method", "GET"},
			{"x-request-id", string(rune('0' + i))},
		}
		encoded, err := encoder.Encode(headers)
		if err != nil {
			t.Fatalf("Encode %d: %v", i, err)
		}
		if len(encoded) == 0 {
			t.Fatalf("Encode %d: empty result", i)
		}
	}
}
