// Package detect provides HTTP protocol detection from initial connection bytes.
package detect

import (
	"errors"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/frame"
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
//  1. If len(buf) < 4, return ErrInsufficientData.
//  2. If first 4 bytes are "PRI ", this may be HTTP/2. Need 24 bytes total.
//     If we have 24 bytes and they match ClientPreface -> engine.H2C
//     If we have < 24 bytes -> ErrInsufficientData (need more data)
//     If 24 bytes don't match -> ErrUnknownProtocol
//  3. If first byte is a known HTTP method start character -> engine.HTTP1
//  4. Otherwise -> ErrUnknownProtocol
func Detect(buf []byte) (engine.Protocol, error) {
	if len(buf) < MinPeekBytes {
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

	return 0, ErrUnknownProtocol
}

// isHTTPMethodByte returns true if b is the first byte of a known HTTP method.
// GET, POST, PUT, DELETE, HEAD, OPTIONS, PATCH, TRACE, CONNECT
func isHTTPMethodByte(b byte) bool {
	switch b {
	case 'G', 'P', 'H', 'D', 'O', 'T', 'C':
		return true
	}
	return false
}
