package h1

import (
	"bytes"
	"strings"
	"unsafe"
)

var (
	bGET     = []byte("GET")
	bPOST    = []byte("POST")
	bPUT     = []byte("PUT")
	bDELETE  = []byte("DELETE")
	bPATCH   = []byte("PATCH")
	bHEAD    = []byte("HEAD")
	bOPTIONS = []byte("OPTIONS")
	bHTTP11  = []byte("HTTP/1.1")
	bHTTP10  = []byte("HTTP/1.0")
	bRoot    = []byte("/")
	bCRLF    = []byte("\r\n")
)

var (
	sGET     = "GET"
	sPOST    = "POST"
	sPUT     = "PUT"
	sDELETE  = "DELETE"
	sPATCH   = "PATCH"
	sHEAD    = "HEAD"
	sOPTIONS = "OPTIONS"
	sHTTP11  = "HTTP/1.1"
	sHTTP10  = "HTTP/1.0"
	sRoot    = "/"
)

func internMethod(b []byte) string {
	switch {
	case bytes.Equal(b, bGET):
		return sGET
	case bytes.Equal(b, bPOST):
		return sPOST
	case bytes.Equal(b, bPUT):
		return sPUT
	case bytes.Equal(b, bDELETE):
		return sDELETE
	case bytes.Equal(b, bPATCH):
		return sPATCH
	case bytes.Equal(b, bHEAD):
		return sHEAD
	case bytes.Equal(b, bOPTIONS):
		return sOPTIONS
	default:
		return string(b)
	}
}

func internVersion(b []byte) string {
	switch {
	case bytes.Equal(b, bHTTP11):
		return sHTTP11
	case bytes.Equal(b, bHTTP10):
		return sHTTP10
	default:
		return string(b)
	}
}

func internPath(b []byte) string {
	if bytes.Equal(b, bRoot) {
		return sRoot
	}
	return string(b)
}

// LowerInPlace lowercases ASCII bytes in-place.
func LowerInPlace(b []byte) {
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 0x20
		}
	}
}

// UnsafeString returns a string that shares the underlying byte slice memory.
// The caller must ensure the byte slice is not modified while the string is in use.
func UnsafeString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// UnsafeLowerHeader returns an interned string for common HTTP header names,
// or lowercases the name in-place and returns a zero-copy unsafe string.
// The caller must ensure the byte slice outlives the returned string.
func UnsafeLowerHeader(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] | 0x20 {
	case 'a':
		if asciiEqualFold(raw, "Accept") {
			return "accept"
		}
		if asciiEqualFold(raw, "Accept-Encoding") {
			return "accept-encoding"
		}
		if asciiEqualFold(raw, "Accept-Language") {
			return "accept-language"
		}
		if asciiEqualFold(raw, "Authorization") {
			return "authorization"
		}
	case 'c':
		if asciiEqualFold(raw, "Content-Type") {
			return "content-type"
		}
		if asciiEqualFold(raw, "Content-Length") {
			return "content-length"
		}
		if asciiEqualFold(raw, "Connection") {
			return "connection"
		}
		if asciiEqualFold(raw, "Cookie") {
			return "cookie"
		}
		if asciiEqualFold(raw, "Cache-Control") {
			return "cache-control"
		}
	case 'e':
		if asciiEqualFold(raw, "Expect") {
			return "expect"
		}
	case 'h':
		if asciiEqualFold(raw, "Host") {
			return "host"
		}
	case 'i':
		if asciiEqualFold(raw, "If-None-Match") {
			return "if-none-match"
		}
		if asciiEqualFold(raw, "If-Modified-Since") {
			return "if-modified-since"
		}
	case 'o':
		if asciiEqualFold(raw, "Origin") {
			return "origin"
		}
	case 'r':
		if asciiEqualFold(raw, "Referer") {
			return "referer"
		}
	case 't':
		if asciiEqualFold(raw, "Transfer-Encoding") {
			return "transfer-encoding"
		}
	case 'u':
		if asciiEqualFold(raw, "User-Agent") {
			return "user-agent"
		}
		if asciiEqualFold(raw, "Upgrade") {
			return "upgrade"
		}
	case 'x':
		if asciiEqualFold(raw, "X-Forwarded-For") {
			return "x-forwarded-for"
		}
		if asciiEqualFold(raw, "X-Real-IP") {
			return "x-real-ip"
		}
		if asciiEqualFold(raw, "X-Request-ID") {
			return "x-request-id"
		}
	}
	LowerInPlace(raw)
	return UnsafeString(raw)
}

// internOrLowerHeader returns a pre-allocated lowercase string for common HTTP
// header names, avoiding the allocation from strings.ToLower(string(raw)).
func internOrLowerHeader(raw []byte) string {
	// Fast path: match by first byte, then verify full name.
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] | 0x20 { // lowercase first byte
	case 'a':
		if asciiEqualFold(raw, "Accept") {
			return "accept"
		}
		if asciiEqualFold(raw, "Accept-Encoding") {
			return "accept-encoding"
		}
		if asciiEqualFold(raw, "Accept-Language") {
			return "accept-language"
		}
		if asciiEqualFold(raw, "Authorization") {
			return "authorization"
		}
	case 'c':
		if asciiEqualFold(raw, "Content-Type") {
			return "content-type"
		}
		if asciiEqualFold(raw, "Content-Length") {
			return "content-length"
		}
		if asciiEqualFold(raw, "Connection") {
			return "connection"
		}
		if asciiEqualFold(raw, "Cookie") {
			return "cookie"
		}
		if asciiEqualFold(raw, "Cache-Control") {
			return "cache-control"
		}
	case 'e':
		if asciiEqualFold(raw, "Expect") {
			return "expect"
		}
	case 'h':
		if asciiEqualFold(raw, "Host") {
			return "host"
		}
	case 'i':
		if asciiEqualFold(raw, "If-None-Match") {
			return "if-none-match"
		}
		if asciiEqualFold(raw, "If-Modified-Since") {
			return "if-modified-since"
		}
	case 'o':
		if asciiEqualFold(raw, "Origin") {
			return "origin"
		}
	case 'r':
		if asciiEqualFold(raw, "Referer") {
			return "referer"
		}
	case 't':
		if asciiEqualFold(raw, "Transfer-Encoding") {
			return "transfer-encoding"
		}
	case 'u':
		if asciiEqualFold(raw, "User-Agent") {
			return "user-agent"
		}
		if asciiEqualFold(raw, "Upgrade") {
			return "upgrade"
		}
	case 'x':
		if asciiEqualFold(raw, "X-Forwarded-For") {
			return "x-forwarded-for"
		}
		if asciiEqualFold(raw, "X-Real-IP") {
			return "x-real-ip"
		}
		if asciiEqualFold(raw, "X-Request-ID") {
			return "x-request-id"
		}
	}
	return strings.ToLower(string(raw))
}

