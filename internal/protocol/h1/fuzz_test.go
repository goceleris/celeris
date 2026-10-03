package h1

import (
	"testing"
)

func FuzzParseRequest(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	f.Add([]byte("POST /path HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhello"))
	f.Add([]byte("DELETE /x HTTP/1.0\r\nHost: h\r\nConnection: close\r\n\r\n"))
	f.Add([]byte(""))
	f.Add([]byte("\r\n\r\n"))
	f.Add([]byte("GET"))
	f.Add([]byte("GET / HTTP/1.1\r\n"))
	f.Add([]byte("GET / HTTP/1.1\r\nBadHeader\r\n\r\n"))
	// Adversarial seeds for the Part 4 smuggling fixes.
	f.Add([]byte("POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked, gzip\r\n\r\n"))
	f.Add([]byte("POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\n"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: h\r\nX-Foo : v\r\n\r\n"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: h\r\nX-Foo: a\x00b\r\n\r\n"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: h\r\nX-Foo: a\rb\r\n\r\n"))

	f.Fuzz(func(_ *testing.T, data []byte) {
		p := NewParser()
		p.Reset(data)
		var req Request
		_, _ = p.ParseRequest(&req)
	})
}

func FuzzParseChunkedBody(f *testing.F) {
	f.Add([]byte("5\r\nhello\r\n0\r\n\r\n"))
	f.Add([]byte("0\r\n\r\n"))
	f.Add([]byte("a\r\n0123456789\r\n0\r\n\r\n"))
	f.Add([]byte(""))
	f.Add([]byte("xyz\r\n"))
	f.Add([]byte("5;ext\r\nhello\r\n"))
	// Adversarial seeds for the trailer + terminator fixes (4.1, 4.4).
	f.Add([]byte("0\r\nX-Trailer: v\r\n\r\n"))
	f.Add([]byte("5\r\nhello\r\n0\r\nT: 1\r\n\r\nGET / HTTP/1.1\r\n"))
	f.Add([]byte("0\r\nX-Trailer: v\r\n"))
	f.Add([]byte("5\r\nhelloXY"))

	f.Fuzz(func(_ *testing.T, data []byte) {
		p := NewParser()
		p.Reset(data)
		_, _, _ = p.ParseChunkedBody()
	})
}
