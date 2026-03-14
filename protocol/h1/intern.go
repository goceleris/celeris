package h1

import (
	"strings"
	"unsafe"
)

var (
	bCRLF = []byte("\r\n")
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
	switch len(b) {
	case 3:
		if b[0] == 'G' && b[1] == 'E' && b[2] == 'T' {
			return sGET
		}
		if b[0] == 'P' && b[1] == 'U' && b[2] == 'T' {
			return sPUT
		}
	case 4:
		if b[0] == 'P' && b[1] == 'O' && b[2] == 'S' && b[3] == 'T' {
			return sPOST
		}
		if b[0] == 'H' && b[1] == 'E' && b[2] == 'A' && b[3] == 'D' {
			return sHEAD
		}
	case 5:
		if b[0] == 'P' && b[1] == 'A' && b[2] == 'T' && b[3] == 'C' && b[4] == 'H' {
			return sPATCH
		}
	case 6:
		if b[0] == 'D' && b[1] == 'E' && b[2] == 'L' && b[3] == 'E' && b[4] == 'T' && b[5] == 'E' {
			return sDELETE
		}
	case 7:
		if b[0] == 'O' && b[1] == 'P' && b[2] == 'T' && b[3] == 'I' && b[4] == 'O' && b[5] == 'N' && b[6] == 'S' {
			return sOPTIONS
		}
	}
	return UnsafeString(b)
}

func internVersion(b []byte) string {
	if len(b) == 8 && b[0] == 'H' && b[5] == '1' && b[6] == '.' {
		if b[7] == '1' {
			return sHTTP11
		}
		if b[7] == '0' {
			return sHTTP10
		}
	}
	return UnsafeString(b)
}

func internPath(b []byte) string {
	if len(b) == 1 && b[0] == '/' {
		return sRoot
	}
	// Zero-copy: path string shares the parser buffer memory.
	// Safe because H1 handlers run synchronously before the buffer is reused.
	return UnsafeString(b)
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
