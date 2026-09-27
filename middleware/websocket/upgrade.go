//line upgrade.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
func computeAcceptKey(key string) string {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[0], 1);
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// validateUpgrade checks that the request is a valid WebSocket upgrade request
// per RFC 6455 Section 4.2.1. Returns the Sec-WebSocket-Key or an error.
func validateUpgrade(c *celeris.Context) (key string, err error) {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[1], 1);
	// Must be GET.
	if c.Method() != "GET" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[7], 1);
		return "", ErrProtocol
	}

	// Connection header must contain "upgrade".
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[2], 1);conn := c.Header("connection")
	if !headerContains(conn, "upgrade") {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[8], 1);
		return "", ErrProtocol
	}

	// Upgrade header must be "websocket" (case-insensitive).
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[3], 1);if !strings.EqualFold(c.Header("upgrade"), "websocket") {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[9], 1);
		return "", ErrProtocol
	}

	// Sec-WebSocket-Version must be "13".
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[4], 1);if c.Header("sec-websocket-version") != "13" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[10], 1);
		return "", ErrProtocol
	}

	// Sec-WebSocket-Key must be present (16 bytes base64-encoded = 24 chars).
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[5], 1);key = c.Header("sec-websocket-key")
	if key == "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[11], 1);
		return "", ErrProtocol
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[6], 1);return key, nil
}

// negotiateSubprotocol selects the first server-supported subprotocol that
// the client also requested. Subprotocol tokens are case-sensitive per
// RFC 6455 Section 4.3.
func negotiateSubprotocol(clientHeader string, serverProtocols []string) string {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[12], 1);
	if len(serverProtocols) == 0 || clientHeader == "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[15], 1);
		return ""
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[13], 1);for _, sp := range serverProtocols {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[16], 1);
		if headerContainsExact(clientHeader, sp) {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[17], 1);
			return sp
		}
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[14], 1);return ""
}

// headerContainsExact checks if a comma-separated header value contains the
// given token with case-sensitive comparison (for subprotocol negotiation).
func headerContainsExact(header, token string) bool {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[18], 1);
	for header != "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[20], 1);
		var item string
		if i := strings.IndexByte(header, ','); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[22], 1);
			item = header[:i]
			header = header[i+1:]
		} else{ _cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[23], 1);{
			item = header
			header = ""
		}}
		_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[21], 1);item = strings.TrimSpace(item)
		if item == token {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[24], 1);
			return true
		}
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[19], 1);return false
}

// headerContains checks if a comma-separated header value contains the given
// token (case-insensitive, per RFC 7230 Section 3.2.6).
func headerContains(header, token string) bool {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[25], 1);
	for header != "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[27], 1);
		var item string
		if i := strings.IndexByte(header, ','); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[29], 1);
			item = header[:i]
			header = header[i+1:]
		} else{ _cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[30], 1);{
			item = header
			header = ""
		}}
		_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[28], 1);item = strings.TrimSpace(item)
		if strings.EqualFold(item, token) {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[31], 1);
			return true
		}
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[26], 1);return false
}

// checkSameOrigin implements the default same-origin check for WebSocket
// upgrade requests. Returns true if the Origin header's host matches the
// request Host header.
func checkSameOrigin(origin, host string) bool {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[32], 1);
	// Extract host from origin URL (e.g. "https://example.com" → "example.com").
	if i := strings.Index(origin, "://"); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[36], 1);
		origin = origin[i+3:]
	}
	// Strip port from both.
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[33], 1);if i := strings.LastIndexByte(origin, ':'); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[37], 1);
		origin = origin[:i]
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[34], 1);if i := strings.LastIndexByte(host, ':'); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[38], 1);
		host = host[:i]
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[35], 1);return strings.EqualFold(origin, host)
}

// negotiateCompression checks if the client requested permessage-deflate
// and the server has it enabled.
func negotiateCompression(clientExtensions string, serverEnabled bool) bool {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[39], 1);
	if !serverEnabled || clientExtensions == "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[42], 1);
		return false
	}
	// Check for "permessage-deflate" in the comma-separated extensions header.
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[40], 1);for clientExtensions != "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[43], 1);
		var ext string
		if i := strings.IndexByte(clientExtensions, ','); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[47], 1);
			ext = clientExtensions[:i]
			clientExtensions = clientExtensions[i+1:]
		} else{ _cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[48], 1);{
			ext = clientExtensions
			clientExtensions = ""
		}}
		_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[44], 1);ext = strings.TrimSpace(ext)
		// Extension may have parameters: "permessage-deflate; server_no_context_takeover"
		_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[45], 1);name := ext
		if i := strings.IndexByte(ext, ';'); i >= 0 {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[49], 1);
			name = strings.TrimSpace(ext[:i])
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[46], 1);if strings.EqualFold(name, "permessage-deflate") {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[50], 1);
			return true
		}
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[41], 1);return false
}

// buildUpgradeResponse builds the raw HTTP 101 response bytes for the
// WebSocket upgrade.
func buildUpgradeResponse(acceptKey, subprotocol string, compress bool) []byte {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[51], 1);
	var b []byte
	b = append(b, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "...)
	b = append(b, acceptKey...)
	b = append(b, "\r\n"...)
	if subprotocol != "" {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[54], 1);
		b = append(b, "Sec-WebSocket-Protocol: "...)
		b = append(b, subprotocol...)
		b = append(b, "\r\n"...)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[52], 1);if compress {_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[55], 1);
		b = append(b, "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"...)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_upgrade.Count[53], 1);b = append(b, "\r\n"...)
	return b
}

var GoCover_b2b_upgrade = struct {
	Count     [56]uint32
	Pos       [3 * 56]uint32
	NumStmt   [56]uint16
} {
	Pos: [3 * 56]uint32{
		17, 21, 0x10002, // [0]
		27, 27, 0x190002, // [1]
		32, 33, 0x260002, // [2]
		38, 38, 0x3a0002, // [3]
		43, 43, 0x2f0002, // [4]
		48, 49, 0xf0002, // [5]
		53, 53, 0x110002, // [6]
		28, 29, 0x10003, // [7]
		34, 35, 0x10003, // [8]
		39, 40, 0x10003, // [9]
		44, 45, 0x10003, // [10]
		50, 51, 0x10003, // [11]
		60, 60, 0x350002, // [12]
		63, 63, 0x250002, // [13]
		68, 68, 0xb0002, // [14]
		61, 62, 0x10003, // [15]
		64, 64, 0x2c0003, // [16]
		65, 66, 0x10004, // [17]
		74, 74, 0x130002, // [18]
		88, 88, 0xe0002, // [19]
		75, 76, 0x320003, // [20]
		83, 84, 0x140003, // [21]
		77, 79, 0x10004, // [22]
		80, 82, 0x10004, // [23]
		85, 86, 0x10004, // [24]
		94, 94, 0x130002, // [25]
		108, 108, 0xe0002, // [26]
		95, 96, 0x320003, // [27]
		103, 104, 0x250003, // [28]
		97, 99, 0x10004, // [29]
		100, 102, 0x10004, // [30]
		105, 106, 0x10004, // [31]
		116, 116, 0x2f0002, // [32]
		120, 120, 0x350002, // [33]
		123, 123, 0x330002, // [34]
		126, 126, 0x280002, // [35]
		117, 118, 0x10003, // [36]
		121, 122, 0x10003, // [37]
		124, 125, 0x10003, // [38]
		132, 132, 0x2e0002, // [39]
		136, 136, 0x1d0002, // [40]
		155, 155, 0xe0002, // [41]
		133, 134, 0x10003, // [42]
		137, 138, 0x3c0003, // [43]
		145, 146, 0x10003, // [44]
		147, 148, 0x2f0003, // [45]
		151, 151, 0x340003, // [46]
		139, 141, 0x10004, // [47]
		142, 144, 0x10004, // [48]
		149, 150, 0x10004, // [49]
		152, 153, 0x10004, // [50]
		161, 165, 0x170002, // [51]
		170, 170, 0xe0002, // [52]
		173, 174, 0xa0002, // [53]
		166, 169, 0x10003, // [54]
		171, 172, 0x10003, // [55]
	},
	NumStmt: [56]uint16{
		4, // 0
		1, // 1
		2, // 2
		1, // 3
		1, // 4
		2, // 5
		1, // 6
		1, // 7
		1, // 8
		1, // 9
		1, // 10
		1, // 11
		1, // 12
		1, // 13
		1, // 14
		1, // 15
		1, // 16
		1, // 17
		1, // 18
		1, // 19
		2, // 20
		2, // 21
		2, // 22
		2, // 23
		1, // 24
		1, // 25
		1, // 26
		2, // 27
		2, // 28
		2, // 29
		2, // 30
		1, // 31
		1, // 32
		1, // 33
		1, // 34
		1, // 35
		1, // 36
		1, // 37
		1, // 38
		1, // 39
		1, // 40
		1, // 41
		1, // 42
		2, // 43
		3, // 44
		3, // 45
		1, // 46
		2, // 47
		2, // 48
		1, // 49
		1, // 50
		5, // 51
		1, // 52
		2, // 53
		3, // 54
		1, // 55
	},
}

var _ = _cover_atomic_.LoadUint32
