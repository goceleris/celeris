package detect

import (
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/frame"
)

func TestDetectHTTP1Methods(t *testing.T) {
	methods := []string{
		"GET / HTTP/1.1\r\n",
		"POST /api HTTP/1.1\r\n",
		"PUT /data HTTP/1.1\r\n",
		"DELETE /resource HTTP/1.1\r\n",
		"HEAD / HTTP/1.1\r\n",
		"OPTIONS * HTTP/1.1\r\n",
		"TRACE / HTTP/1.1\r\n",
		"CONNECT host:443 HTTP/1.1\r\n",
	}
	for _, m := range methods {
		proto, err := Detect([]byte(m))
		if err != nil {
			t.Errorf("Detect(%q): unexpected error: %v", m[:4], err)
		}
		if proto != engine.HTTP1 {
			t.Errorf("Detect(%q): got %v, want HTTP1", m[:4], proto)
		}
	}
}

func TestDetectH2C(t *testing.T) {
	proto, err := Detect([]byte(frame.ClientPreface))
	if err != nil {
		t.Fatalf("Detect(h2 preface): unexpected error: %v", err)
	}
	if proto != engine.H2C {
		t.Fatalf("Detect(h2 preface): got %v, want H2C", proto)
	}
}

func TestDetectH2CWithExtra(t *testing.T) {
	buf := append([]byte(frame.ClientPreface), 0x00, 0x00, 0x00)
	proto, err := Detect(buf)
	if err != nil {
		t.Fatalf("Detect(h2 preface+extra): unexpected error: %v", err)
	}
	if proto != engine.H2C {
		t.Fatalf("Detect(h2 preface+extra): got %v, want H2C", proto)
	}
}

func TestDetectInsufficientData(t *testing.T) {
	tests := [][]byte{
		nil,
		{},
		{'G'},
		{'G', 'E', 'T'},
		// Partial H2 preface (only 4 bytes of "PRI ")
		[]byte("PRI "),
		[]byte("PRI * HTTP/2.0\r\n"),
	}
	for _, buf := range tests {
		_, err := Detect(buf)
		if err != ErrInsufficientData {
			t.Errorf("Detect(%q): got err=%v, want ErrInsufficientData", buf, err)
		}
	}
}

func TestDetectUnknown(t *testing.T) {
	tests := [][]byte{
		{0x00, 0x00, 0x00, 0x00},
		{0xFF, 0xFE, 0xFD, 0xFC},
		[]byte("XGET /"),
	}
	for _, buf := range tests {
		_, err := Detect(buf)
		if err != ErrUnknownProtocol {
			t.Errorf("Detect(%q): got err=%v, want ErrUnknownProtocol", buf, err)
		}
	}
}

func TestDetectBadH2Preface(t *testing.T) {
	// Full length but wrong content
	buf := make([]byte, PrefaceLen)
	copy(buf, "PRI * HTTP/2.0\r\n\r\nXX\r\n\r\n")
	_, err := Detect(buf)
	if err != ErrUnknownProtocol {
		t.Errorf("Detect(bad h2 preface): got err=%v, want ErrUnknownProtocol", err)
	}
}

func BenchmarkDetectHTTP1(b *testing.B) {
	buf := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	for b.Loop() {
		_, _ = Detect(buf)
	}
}

func BenchmarkDetectH2C(b *testing.B) {
	buf := []byte(frame.ClientPreface)
	for b.Loop() {
		_, _ = Detect(buf)
	}
}
