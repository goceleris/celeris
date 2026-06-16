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

// BenchmarkParseRequest_KeepAliveGET_H2CDetect mirrors the engine hot path:
// zero-copy headers with h2c-upgrade detection enabled (the default Auto config,
// EnableH2Upgrade=true). The request carries a realistic browser-ish header set
// where most field-names cannot begin an h2c signal (Upgrade/HTTP2-Settings/
// Connection), so the per-header detection call is pure overhead on these.
func BenchmarkParseRequest_KeepAliveGET_H2CDetect(b *testing.B) {
	raw := []byte("GET /api/users/42 HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/140.0\r\n" +
		"Accept: application/json,text/plain,*/*\r\n" +
		"Accept-Encoding: gzip, deflate, br\r\n" +
		"Accept-Language: en-US,en;q=0.9\r\n" +
		"Referer: https://example.com/dashboard\r\n" +
		"Connection: keep-alive\r\n\r\n")
	p := NewParser()
	p.SetZeroCopy(true) // engines parse in zero-copy mode
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

func BenchmarkFindHeaderEnd(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			// Place \r\n\r\n at the end
			buf := make([]byte, size)
			for i := range buf {
				buf[i] = 'A'
			}
			copy(buf[size-4:], "\r\n\r\n")
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				findHeaderEnd(buf)
			}
		})
	}
}

func BenchmarkFindHeaderEnd_8K(b *testing.B) {
	size := 8192
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = 'A'
	}
	copy(buf[size-4:], "\r\n\r\n")
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		findHeaderEnd(buf)
	}
}
