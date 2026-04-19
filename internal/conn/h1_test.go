package conn

import (
	"testing"

	"github.com/goceleris/celeris/protocol/h1"
)

func TestH1StateMaxBodySize(t *testing.T) {
	s := NewH1State()

	// Zero value = unlimited (0 passes through, limit > 0 guard disables enforcement)
	if got := s.maxBodySize(); got != 0 {
		t.Fatalf("expected 0 (unlimited), got %d", got)
	}

	// Custom value
	s.MaxRequestBodySize = 50 << 20
	if got := s.maxBodySize(); got != 50<<20 {
		t.Fatalf("expected 50MB, got %d", got)
	}
}

// TestH2CUpgradeHeaderValuesStable verifies copyH2CHeaders returns
// heap-owned strings that do NOT alias the H1 recv buffer. Before the fix
// the returned header names came from h1.UnsafeLowerHeader which, for
// uncommon names, returns a zero-copy unsafe.String over the caller's
// buffer — corrupting the H2 stream 1 header list if the buffer is
// reused after switchToH2.
func TestH2CUpgradeHeaderValuesStable(t *testing.T) {
	// Use an uncommon header name so internOrLowerHeader takes the
	// strings.ToLower path (not a pre-allocated literal). Note
	// UnsafeLowerHeader would take the LowerInPlace+UnsafeString path.
	rawName := []byte("X-Custom-Header-Name")
	rawValue := []byte("custom-value-42")

	req := h1.Request{
		RawHeaders: [][2][]byte{
			{rawName, rawValue},
		},
	}

	out := copyH2CHeaders(&req)
	if len(out) != 1 {
		t.Fatalf("got %d headers, want 1", len(out))
	}
	gotName := out[0][0]
	gotVal := out[0][1]
	if gotName != "x-custom-header-name" {
		t.Fatalf("name = %q, want x-custom-header-name", gotName)
	}
	if gotVal != "custom-value-42" {
		t.Fatalf("value = %q, want custom-value-42", gotVal)
	}

	// Scribble the backing buffers (simulating the driver reading fresh
	// bytes into the recv buffer after switchToH2 releases H1 state).
	for i := range rawName {
		rawName[i] = 'Z'
	}
	for i := range rawValue {
		rawValue[i] = 'Z'
	}

	// The returned strings MUST be unaffected — otherwise the H2 stream 1
	// headers are corrupted by the time the H2 processor reads them.
	if gotName != "x-custom-header-name" {
		t.Fatalf("header name aliased recv buffer: got %q after scribble", gotName)
	}
	if gotVal != "custom-value-42" {
		t.Fatalf("header value aliased recv buffer: got %q after scribble", gotVal)
	}
}

// TestSwitchToH2ErrorStateClean exercises the copyH2CHeaders hop-by-hop
// filter to ensure upgrade/connection/http2-settings are stripped when
// NewH2StateFromUpgrade is called. A matching filter is required for the
// reordering fix in switchToH2 (build H2 state first, release H1 state on
// success) to leave cs in a clean state when NewH2StateFromUpgrade fails
// — the headers we pass must not contain hop-by-hop entries that would
// surface as H2 protocol errors.
func TestSwitchToH2ErrorStateClean(t *testing.T) {
	rawHeaders := [][2][]byte{
		{[]byte("Host"), []byte("example.com")},
		{[]byte("Upgrade"), []byte("h2c")},
		{[]byte("Connection"), []byte("Upgrade, HTTP2-Settings")},
		{[]byte("HTTP2-Settings"), []byte("abc")},
		{[]byte("User-Agent"), []byte("test")},
	}
	req := h1.Request{RawHeaders: rawHeaders}
	out := copyH2CHeaders(&req)

	for _, h := range out {
		switch h[0] {
		case "upgrade", "connection", "http2-settings":
			t.Fatalf("hop-by-hop header %q leaked into H2 header list", h[0])
		}
	}

	// Host must be preserved (rewritten to :authority by caller).
	foundHost := false
	foundUA := false
	for _, h := range out {
		if h[0] == "host" && h[1] == "example.com" {
			foundHost = true
		}
		if h[0] == "user-agent" && h[1] == "test" {
			foundUA = true
		}
	}
	if !foundHost {
		t.Fatal("host header missing from copied list")
	}
	if !foundUA {
		t.Fatal("user-agent header missing from copied list")
	}
}
