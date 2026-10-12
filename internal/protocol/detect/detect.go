// Package detect provides HTTP protocol detection from initial connection bytes.
package detect

import (
	"errors"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/frame"
)

// MinPeekBytes is the minimum number of bytes needed to begin detection.
const MinPeekBytes = 4

// PrefaceLen is the full HTTP/2 client connection preface length.
const PrefaceLen = len(frame.ClientPreface)

var (
	// ErrInsufficientData indicates not enough bytes for detection.
	ErrInsufficientData = errors.New("insufficient data for protocol detection")
	// ErrUnknownProtocol indicates the data does not match any known protocol.
	ErrUnknownProtocol = errors.New("unknown protocol")
)

// Detect examines the initial bytes of a connection and returns the detected protocol.
//
// Detection logic:
//  1. If the first two bytes are CRLF, return engine.HTTP1 at once, whatever
//     follows. RFC 9112 section 2.2: a server SHOULD ignore at least one empty
//     line received before the request line, and the HTTP/1 parser does. The
//     decision does not depend on the bytes after the CRLF, so what detection
//     can hold stays under PrefaceLen.
//  2. If len(buf) < 4, return ErrInsufficientData.
//  3. If first 4 bytes are "PRI ", this may be HTTP/2. Need 24 bytes total.
//     If we have 24 bytes and they match ClientPreface -> engine.H2C
//     If we have < 24 bytes -> ErrInsufficientData (need more data)
//     If 24 bytes don't match -> ErrUnknownProtocol
//  4. If first byte starts a method of the IANA HTTP Method Registry ->
//     engine.HTTP1
//  5. Otherwise -> ErrUnknownProtocol
func Detect(buf []byte) (engine.Protocol, error) {
	if len(buf) < MinPeekBytes {
		if len(buf) >= 2 && buf[0] == '\r' && buf[1] == '\n' {
			return engine.HTTP1, nil
		}
		return 0, ErrInsufficientData
	}

	// Check for HTTP/2 client preface
	if buf[0] == 'P' && buf[1] == 'R' && buf[2] == 'I' && buf[3] == ' ' {
		if len(buf) < PrefaceLen {
			return 0, ErrInsufficientData
		}
		if string(buf[:PrefaceLen]) == frame.ClientPreface {
			return engine.H2C, nil
		}
		return 0, ErrUnknownProtocol
	}

	// Check for HTTP/1.x method start characters
	if isHTTPMethodByte(buf[0]) {
		return engine.HTTP1, nil
	}

	// An empty line before the request line (RFC 9112 section 2.2).
	if buf[0] == '\r' && buf[1] == '\n' {
		return engine.HTTP1, nil
	}

	return 0, ErrUnknownProtocol
}

// isHTTPMethodByte returns true if b is the first byte of a method in the IANA
// HTTP Method Registry (https://www.iana.org/assignments/http-methods): ACL,
// BASELINE-CONTROL, BIND, CHECKIN, CHECKOUT, CONNECT, COPY, DELETE, GET, HEAD,
// LABEL, LINK, LOCK, MERGE, MKACTIVITY, MKCALENDAR, MKCOL, MKREDIRECTREF,
// MKWORKSPACE, MOVE, OPTIONS, ORDERPATCH, PATCH, POST, PROPFIND, PROPPATCH,
// PUT, QUERY, REBIND, REPORT, SEARCH, TRACE, UNBIND, UNCHECKOUT, UNLINK,
// UNLOCK, UPDATE, UPDATEREDIRECTREF and VERSION-CONTROL ("PRI" is read as the
// start of the HTTP/2 preface before this is asked). net/http accepts any
// token as a method; a first letter outside this set (E F I J K N W X Y Z, a
// digit, a control byte such as a TLS record header) is not HTTP for Auto mode.
func isHTTPMethodByte(b byte) bool {
	switch b {
	case 'A', 'B', 'C', 'D', 'G', 'H', 'L', 'M', 'O', 'P', 'Q', 'R', 'S', 'T', 'U', 'V':
		return true
	}
	return false
}
