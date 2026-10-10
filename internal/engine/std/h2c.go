// Copyright 2009 The Go Authors.
//
// The handler below is derived from golang.org/x/net/http2/h2c (copyright 2018
// The Go Authors); that module is licensed as follows.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google LLC nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package std

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/textproto"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

// h2cHandler is the std engine's h2c front end: it intercepts the two ways an
// HTTP/2 connection begins on a cleartext HTTP/1 listener (RFC 7540 3.4, prior
// knowledge, and 3.2, the Upgrade handshake), hijacks the connection and
// serves it through h2s.ServeConn, and passes every other request on to next.
//
// It is golang.org/x/net/http2/h2c's handler (Copyright 2018 The Go
// Authors, BSD-3-Clause), with one difference, which is why it is not that
// package's: on go1.27 it calls ServeConn with no BaseConfig (celeris#878;
// h2cBaseConfig says what the other builds pass). h2c passes
// the http.Server the request came in on, and x/net then serves the
// connection through a one-off http.Server built for it alone (go1.27 and
// later, where x/net/http2 wraps net/http's HTTP/2 server; server_wrap.go),
// which nothing can ever shut down. With no BaseConfig the connection is
// served through the server h2s was registered with (http2.ConfigureServer in
// New), and http.Server.Shutdown reaches it: net/http stops tracking a hijacked
// connection, but the GOAWAY hook registered on that server still does.
// The connection is thereby served under the engine's own Config limits
// (ReadTimeout, WriteTimeout, MaxHeaderBytes) and, since h2s.IdleTimeout is
// copied from the server by ConfigureServer, IdleTimeout, which an h2c
// connection used to be exempt from. Detection and the 500 on a failed
// upgrade are unchanged. The body of an upgrade request is read as before,
// into memory before the connection is taken over, but no further than
// MaxRequestBodySize (celeris#976).
type h2cHandler struct {
	next http.Handler
	h2s  *http2.Server //nolint:staticcheck // SA1019: the type ServeConn is a method of; see the import comment in engine.go.
	// maxBody is Config.MaxRequestBodySize, which bounds the body of an
	// upgrade request as it bounds every other (celeris#976); 0 = unlimited.
	maxBody int64
	// bodyRefused counts an upgrade request refused for its body, as
	// Bridge counts a body it refuses (may be nil).
	bodyRefused func()
}

func (h *h2cHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Prior knowledge (RFC 7540 3.4).
	if r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0" {
		conn, err := h2cPriorKnowledge(w)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		h.h2s.ServeConn(conn, &http2.ServeConnOpts{ //nolint:staticcheck // SA1019: see the import comment in engine.go.
			Context:          r.Context(),
			Handler:          h.next,
			BaseConfig:       h2cBaseConfig(r),
			SawClientPreface: true,
		})
		return
	}
	// Upgrade to h2c (RFC 7540 3.2).
	if isH2CUpgrade(r.Header) {
		conn, settings, err := h2cUpgrade(w, r, h.maxBody)
		if err != nil {
			if errors.Is(err, errH2CUpgradeBodyTooLarge) {
				// The connection is not taken over: refuse as Bridge does
				// a body over the limit (celeris#976). Close it after the
				// answer, as the rest of the body is unread.
				if h.bodyRefused != nil {
					h.bodyRefused()
				}
				w.Header().Set("Connection", "close")
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer func() { _ = conn.Close() }()
		h.h2s.ServeConn(conn, &http2.ServeConnOpts{ //nolint:staticcheck // SA1019: see the import comment in engine.go.
			Context:        r.Context(),
			Handler:        h.next,
			BaseConfig:     h2cBaseConfig(r),
			UpgradeRequest: r,
			Settings:       settings,
		})
		return
	}
	h.next.ServeHTTP(w, r)
}

// h2cPriorKnowledge hijacks the connection and checks the rest of the client
// preface, the part of it net/http read as the request "PRI * HTTP/2.0"; the
// returned conn gives ServeConn that part back, as it expects it first.
func h2cPriorKnowledge(w http.ResponseWriter) (net.Conn, error) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	const expectedBody = "SM\r\n\r\n"
	buf := make([]byte, len(expectedBody))
	n, err := io.ReadFull(rw, buf)
	if err != nil {
		return nil, fmt.Errorf("h2c: error reading client preface: %w", err)
	}
	if string(buf[:n]) == expectedBody {
		return newH2CBufConn(conn, rw), nil
	}
	_ = conn.Close()
	return nil, errors.New("h2c: invalid client preface")
}

// errH2CUpgradeBodyTooLarge is h2cUpgrade's error for an upgrade request whose
// body is over Config.MaxRequestBodySize.
var errH2CUpgradeBodyTooLarge = errors.New("h2c: upgrade request body over MaxRequestBodySize")

// h2cUpgrade answers an HTTP/1.1 upgrade request with 101 and hijacks the
// connection (RFC 7540 3.2); settings is the decoded HTTP2-Settings header.
// maxBody is the largest request body it buffers (0 = no limit): a larger one
// is errH2CUpgradeBodyTooLarge, with the connection not taken over (celeris#976).
func h2cUpgrade(w http.ResponseWriter, r *http.Request, maxBody int64) (_ net.Conn, settings []byte, err error) {
	settings, err = h2cSettings(r.Header)
	if err != nil {
		return nil, nil, err
	}
	// The request is served as stream 1, body included: read it before the
	// connection is taken over, but no further than the limit that applies
	// to every request body (Bridge.ServeHTTP reads the same way: one byte
	// past the limit tells "over" from "exactly at"). It used to read all of
	// it, so a client could make the server buffer a body of any size
	// before MaxRequestBodySize was looked at (celeris#976).
	var body []byte
	if maxBody > 0 {
		limit := maxBody
		if limit < math.MaxInt64 { // maxBody+1 wraps for MaxInt64
			limit++
		}
		body, err = io.ReadAll(io.LimitReader(r.Body, limit))
		if err == nil && int64(len(body)) > maxBody {
			return nil, nil, errH2CUpgradeBodyTooLarge
		}
	} else {
		body, err = io.ReadAll(r.Body)
	}
	if err != nil {
		return nil, nil, err
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, nil, err
	}
	_, _ = rw.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: h2c\r\n\r\n"))
	return newH2CBufConn(conn, rw), settings, nil
}

// isH2CUpgrade reports whether the headers properly request an upgrade to h2c
// (RFC 7540 3.2).
func isH2CUpgrade(h http.Header) bool {
	return httpguts.HeaderValuesContainsToken(h[textproto.CanonicalMIMEHeaderKey("Upgrade")], "h2c") &&
		httpguts.HeaderValuesContainsToken(h[textproto.CanonicalMIMEHeaderKey("Connection")], "HTTP2-Settings")
}

// h2cSettings returns the decoded HTTP2-Settings header.
func h2cSettings(h http.Header) ([]byte, error) {
	vals, ok := h[textproto.CanonicalMIMEHeaderKey("HTTP2-Settings")]
	if !ok {
		return nil, errors.New("missing HTTP2-Settings header")
	}
	if len(vals) != 1 {
		return nil, fmt.Errorf("expected 1 HTTP2-Settings. Got: %v", vals)
	}
	return base64.RawURLEncoding.DecodeString(vals[0])
}

// newH2CBufConn flushes rw and returns conn, or, if net/http's reader holds
// bytes it has read ahead, a conn whose reads drain them first.
func newH2CBufConn(conn net.Conn, rw *bufio.ReadWriter) net.Conn {
	_ = rw.Flush()
	if rw.Reader.Buffered() == 0 {
		return conn
	}
	return &h2cBufConn{Conn: conn, Reader: rw.Reader}
}

// h2cBufConn wraps a net.Conn, but reads drain the bufio.Reader first.
type h2cBufConn struct {
	net.Conn
	*bufio.Reader
}

func (c *h2cBufConn) Read(p []byte) (int, error) {
	if c.Reader == nil {
		return c.Conn.Read(p)
	}
	n := c.Buffered()
	if n == 0 {
		c.Reader = nil
		return c.Conn.Read(p)
	}
	if n < len(p) {
		p = p[:n]
	}
	return c.Reader.Read(p)
}
