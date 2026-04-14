package websocket

import (
	"crypto/sha1"
	"encoding/base64"
	"strings"

	"github.com/goceleris/celeris"
)

// websocketGUID is the magic GUID from RFC 6455 Section 4.2.2.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// computeAcceptKey computes the Sec-WebSocket-Accept value from the client's
// Sec-WebSocket-Key per RFC 6455 Section 4.2.2.
func computeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// validateUpgrade checks that the request is a valid WebSocket upgrade request
// per RFC 6455 Section 4.2.1. Returns the Sec-WebSocket-Key or an error.
func validateUpgrade(c *celeris.Context) (key string, err error) {
	// Must be GET.
	if c.Method() != "GET" {
		return "", ErrProtocol
	}

	// Connection header must contain "upgrade".
	conn := c.Header("connection")
	if !headerContains(conn, "upgrade") {
		return "", ErrProtocol
	}

	// Upgrade header must be "websocket" (case-insensitive).
	if !strings.EqualFold(c.Header("upgrade"), "websocket") {
		return "", ErrProtocol
	}

	// Sec-WebSocket-Version must be "13".
	if c.Header("sec-websocket-version") != "13" {
		return "", ErrProtocol
	}

	// Sec-WebSocket-Key must be present (16 bytes base64-encoded = 24 chars).
	key = c.Header("sec-websocket-key")
	if key == "" {
		return "", ErrProtocol
	}

	return key, nil
}

// negotiateSubprotocol selects the first server-supported subprotocol that
// the client also requested. Subprotocol tokens are case-sensitive per
// RFC 6455 Section 4.3.
func negotiateSubprotocol(clientHeader string, serverProtocols []string) string {
	if len(serverProtocols) == 0 || clientHeader == "" {
		return ""
	}
	for _, sp := range serverProtocols {
		if headerContainsExact(clientHeader, sp) {
			return sp
		}
	}
	return ""
}

// headerContainsExact checks if a comma-separated header value contains the
// given token with case-sensitive comparison (for subprotocol negotiation).
func headerContainsExact(header, token string) bool {
	for header != "" {
		var item string
		if i := strings.IndexByte(header, ','); i >= 0 {
			item = header[:i]
			header = header[i+1:]
		} else {
			item = header
			header = ""
		}
		item = strings.TrimSpace(item)
		if item == token {
			return true
		}
	}
	return false
}

// headerContains checks if a comma-separated header value contains the given
// token (case-insensitive, per RFC 7230 Section 3.2.6).
func headerContains(header, token string) bool {
	for header != "" {
		var item string
		if i := strings.IndexByte(header, ','); i >= 0 {
			item = header[:i]
			header = header[i+1:]
		} else {
			item = header
			header = ""
		}
		item = strings.TrimSpace(item)
		if strings.EqualFold(item, token) {
			return true
		}
	}
	return false
}

// checkSameOrigin implements the default same-origin check for WebSocket
// upgrade requests. Returns true if the Origin header's host matches the
// request Host header.
func checkSameOrigin(origin, host string) bool {
	// Extract host from origin URL (e.g. "https://example.com" → "example.com").
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	// Strip port from both.
	if i := strings.LastIndexByte(origin, ':'); i >= 0 {
		origin = origin[:i]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return strings.EqualFold(origin, host)
}

// negotiateCompression checks if the client requested permessage-deflate
// and the server has it enabled.
func negotiateCompression(clientExtensions string, serverEnabled bool) bool {
	if !serverEnabled || clientExtensions == "" {
		return false
	}
	// Check for "permessage-deflate" in the comma-separated extensions header.
	for clientExtensions != "" {
		var ext string
		if i := strings.IndexByte(clientExtensions, ','); i >= 0 {
			ext = clientExtensions[:i]
			clientExtensions = clientExtensions[i+1:]
		} else {
			ext = clientExtensions
			clientExtensions = ""
		}
		ext = strings.TrimSpace(ext)
		// Extension may have parameters: "permessage-deflate; server_no_context_takeover"
		name := ext
		if i := strings.IndexByte(ext, ';'); i >= 0 {
			name = strings.TrimSpace(ext[:i])
		}
		if strings.EqualFold(name, "permessage-deflate") {
			return true
		}
	}
	return false
}

// buildUpgradeResponse builds the raw HTTP 101 response bytes for the
// WebSocket upgrade.
func buildUpgradeResponse(acceptKey, subprotocol string, compress bool) []byte {
	var b []byte
	b = append(b, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "...)
	b = append(b, acceptKey...)
	b = append(b, "\r\n"...)
	if subprotocol != "" {
		b = append(b, "Sec-WebSocket-Protocol: "...)
		b = append(b, subprotocol...)
		b = append(b, "\r\n"...)
	}
	if compress {
		b = append(b, "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"...)
	}
	b = append(b, "\r\n"...)
	return b
}
