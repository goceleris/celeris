package h1

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkParseRequest_SimpleGET(b *testing.B) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(raw)
		req.Reset()
		_, _ = p.ParseRequest(&req)
	}
}

func BenchmarkParseRequest_ManyHeaders(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("POST /api/v1/data HTTP/1.1\r\n")
	sb.WriteString("Host: api.example.com\r\n")
	sb.WriteString("Content-Type: application/json\r\n")
	sb.WriteString("Content-Length: 13\r\n")
	sb.WriteString("Authorization: Bearer token123456789\r\n")
	sb.WriteString("Accept: application/json\r\n")
	sb.WriteString("Accept-Encoding: gzip, deflate\r\n")
	sb.WriteString("Accept-Language: en-US,en;q=0.9\r\n")
	sb.WriteString("User-Agent: BenchClient/1.0\r\n")
	sb.WriteString("X-Request-ID: abc-def-ghi\r\n")
	sb.WriteString("X-Forwarded-For: 10.0.0.1\r\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "X-Custom-%02d: value-%02d\r\n", i, i)
	}
	sb.WriteString("\r\n")
	sb.WriteString("Hello, World!")

	raw := []byte(sb.String())
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(raw)
		req.Reset()
		_, _ = p.ParseRequest(&req)
	}
}

func BenchmarkGetBody_ZeroCopy(b *testing.B) {
	body := strings.Repeat("x", 4096)
	raw := []byte(fmt.Sprintf("POST / HTTP/1.1\r\nHost: h\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(raw)
		req.Reset()
		_, _ = p.ParseRequest(&req)
		p.GetBody(req.ContentLength)
	}
}

func BenchmarkParseRequest_Pipelined(b *testing.B) {
	single := "GET /path HTTP/1.1\r\nHost: example.com\r\n\r\n"
	pipelined := []byte(strings.Repeat(single, 10))
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(pipelined)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(pipelined)
		for j := 0; j < 10; j++ {
			req.Reset()
			_, _ = p.ParseRequest(&req)
		}
	}
}

// BenchmarkParseRequest_LongTarget889 is the request-target scan (celeris#889
// item 45) on a target of realistic length: a REST path with a query string.
func BenchmarkParseRequest_LongTarget889(b *testing.B) {
	raw := []byte("GET /api/v1/users/1234567/orders/98765/items?limit=50&offset=100&sort=created_at:desc&fields=id,total HTTP/1.1\r\nHost: example.com\r\nAccept: application/json\r\n\r\n")
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(raw)
		req.Reset()
		_, _ = p.ParseRequest(&req)
	}
}

// BenchmarkParseRequest_NonInternedMethod889 is the method token scan (item 40),
// which only a method outside the seven interned ones pays.
func BenchmarkParseRequest_NonInternedMethod889(b *testing.B) {
	raw := []byte("PROPFIND /dav/collection HTTP/1.1\r\nHost: example.com\r\n\r\n")
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(raw)
		req.Reset()
		_, _ = p.ParseRequest(&req)
	}
}

// BenchmarkParseRequest_H2CUpgrade889 is a request that carries the h2c upgrade
// headers (cold: only an upgrade request has them).
func BenchmarkParseRequest_H2CUpgrade889(b *testing.B) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n")
	p := NewParser()
	var req Request
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Reset(raw)
		req.Reset()
		_, _ = p.ParseRequest(&req)
	}
}
