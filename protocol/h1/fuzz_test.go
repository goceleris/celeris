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

	f.Fuzz(func(t *testing.T, data []byte) {
		p := NewParser()
		p.Reset(data)
		var req Request
		p.ParseRequest(&req)
	})
}

func FuzzParseChunkedBody(f *testing.F) {
	f.Add([]byte("5\r\nhello\r\n0\r\n\r\n"))
	f.Add([]byte("0\r\n\r\n"))
	f.Add([]byte("a\r\n0123456789\r\n0\r\n\r\n"))
	f.Add([]byte(""))
	f.Add([]byte("xyz\r\n"))
	f.Add([]byte("5;ext\r\nhello\r\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := NewParser()
		p.Reset(data)
		p.ParseChunkedBody()
	})
}
