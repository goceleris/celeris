package conn

import "testing"

func TestPutResponseBufferDiscardsOversized(t *testing.T) {
	// Allocate a buffer larger than the 128KB cap.
	big := make([]byte, 0, 256<<10) // 256 KB
	putResponseBuffer(&big)

	// Get a fresh buffer — it should be the default 32KB, not the oversized one.
	p := getResponseBuffer()
	if cap(*p) > 128<<10 {
		t.Fatalf("expected cap <= 128KB from pool after discarding oversized, got %d", cap(*p))
	}
}

func TestPutResponseBufferKeepsNormal(t *testing.T) {
	// Allocate a buffer within the cap.
	normal := make([]byte, 100, 64<<10) // 64 KB
	putResponseBuffer(&normal)

	// Get it back — should be reset to length 0.
	p := getResponseBuffer()
	if len(*p) != 0 {
		t.Fatalf("expected length 0, got %d", len(*p))
	}
}
