package detect

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/frame"
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

// ianaMethods is the HTTP Method Registry
// (https://www.iana.org/assignments/http-methods/methods.csv, fetched by
// scripts/iana-letters.sh in the lane evidence dir on 2026-10-12), less the
// "*" entry that is not a method and "PRI", which Detect reads as the start of
// the HTTP/2 preface.
var ianaMethods = []string{
	"ACL", "BASELINE-CONTROL", "BIND", "CHECKIN", "CHECKOUT", "CONNECT", "COPY", "DELETE", "GET", "HEAD",
	"LABEL", "LINK", "LOCK", "MERGE", "MKACTIVITY", "MKCALENDAR", "MKCOL", "MKREDIRECTREF", "MKWORKSPACE",
	"MOVE", "OPTIONS", "ORDERPATCH", "PATCH", "POST", "PROPFIND", "PROPPATCH", "PUT", "QUERY", "REBIND",
	"REPORT", "SEARCH", "TRACE", "UNBIND", "UNCHECKOUT", "UNLINK", "UNLOCK", "UPDATE", "UPDATEREDIRECTREF",
	"VERSION-CONTROL",
}

// TestDetectIANAMethods: every method of the IANA registry starts an HTTP/1
// request, not only the nine of RFC 9110 (WebDAV PROPFIND and MKCOL, QUERY and
// SEARCH were closed in Auto mode).
func TestDetectIANAMethods(t *testing.T) {
	for _, m := range ianaMethods {
		buf := []byte(m + " /x HTTP/1.1\r\nHost: h\r\n\r\n")
		proto, err := Detect(buf)
		if err != nil || proto != engine.HTTP1 {
			t.Errorf("Detect(%q...): got %v, %v, want HTTP1, nil", m, proto, err)
		}
	}
}

// TestDetectNotAMethodLetter: a first byte that starts no registered method
// stays unrecognised (the letters E F I J K N W X Y Z, digits, control bytes
// such as a TLS record header), so Auto still closes a conn that is not
// speaking HTTP.
func TestDetectNotAMethodLetter(t *testing.T) {
	for _, first := range []byte("EFIJKNWXYZefgx0123456789\x16\x00\xff \t\n") {
		buf := []byte{first, 'E', 'T', ' ', '/', 'x'}
		if _, err := Detect(buf); err != ErrUnknownProtocol {
			t.Errorf("Detect(%q...): got err=%v, want ErrUnknownProtocol", buf, err)
		}
	}
}

// TestDetectLeadingCRLF: RFC 9112 section 2.2, a server SHOULD ignore at least
// one empty line received before the request line. The empty line decides the
// conn at once, whatever follows it, so what detection can hold stays under
// PrefaceLen (the bound the io_uring accumulator and epoll's held prefix rely
// on).
func TestDetectLeadingCRLF(t *testing.T) {
	decided := []string{
		"\r\n",
		"\r\nG",
		"\r\nGET / HTTP/1.1\r\n",
		"\r\nPROPFIND /x HTTP/1.1\r\n",
		"\r\n\r\n",
		"\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n\r\n",
		"\r\nPRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
		"\r\nXGET /",
	}
	for _, in := range decided {
		proto, err := Detect([]byte(in))
		if err != nil || proto != engine.HTTP1 {
			t.Errorf("Detect(%q): got %v, %v, want HTTP1, nil", in, proto, err)
		}
	}
	// A lone CR waits for its LF; a CR that is not followed by one is not HTTP.
	if _, err := Detect([]byte("\r")); err != ErrInsufficientData {
		t.Errorf(`Detect("\r"): got err=%v, want ErrInsufficientData`, err)
	}
	for _, in := range []string{"\rG", "\rGET /", "\r\r\n", "\n", "\nGET /", "\n\r\n"} {
		if _, err := Detect([]byte(in)); err == nil {
			t.Errorf("Detect(%q): decided, want it undecided or unknown (only CRLF is skipped)", in)
		}
	}
}

// TestDetectDecidesWithinPrefaceLen is the bound the engines rely on: Detect
// never asks for more once it has PrefaceLen bytes, whatever they are, with CR
// and LF among the bytes. The alphabet is small so that the prefixes that
// matter (CRLF runs, "PRI ", method letters) recur.
func TestDetectDecidesWithinPrefaceLen(t *testing.T) {
	alphabet := []byte("\r\nPRI *GXE/")
	var seed uint64 = 1
	next := func() uint64 { seed = seed*6364136223846793005 + 1442695040888963407; return seed >> 33 }
	for i := 0; i < 200000; i++ {
		n := PrefaceLen + int(next()%8)
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = alphabet[next()%uint64(len(alphabet))]
		}
		if _, err := Detect(buf); err == ErrInsufficientData {
			t.Fatalf("Detect(%q) of %d bytes is undecided", buf, n)
		}
	}
	for _, lead := range []string{"\r\n", "\r\n\r\n", "PRI ", "\r\nPRI ", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r"} {
		buf := []byte(lead + strings.Repeat("\r\n", PrefaceLen))
		if _, err := Detect(buf[:PrefaceLen]); err == ErrInsufficientData {
			t.Errorf("Detect(%q...) of %d bytes is undecided", lead, PrefaceLen)
		}
	}
}
