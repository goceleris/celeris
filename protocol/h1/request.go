package h1

// MaxHeaderSize is the maximum allowed size of HTTP headers in bytes.
// 64 KiB matches nginx's large_client_header_buffers ceiling and is
// two orders of magnitude below the previous 16 MiB which was a
// slow-loris amplifier. Legitimate request headers are always well
// under a few KiB; 64 KiB accommodates edge cases like verbose proxy
// chains without exposing the server to per-connection memory
// exhaustion.
const MaxHeaderSize = 64 << 10

// MaxHeaderCount caps the number of header fields in a single
// request. Prevents an attacker from sending thousands of tiny
// headers that stay under the MaxHeaderSize byte limit while
// still triggering O(n) slice growth and scan work. 200 sits
// above typical nginx/apache defaults (100 for client headers,
// but verbose proxy chains can add more) and well below the
// thousands-per-request attack threshold.
const MaxHeaderCount = 200

// Request holds the parsed components of an HTTP/1.x request.
type Request struct {
	Method          string
	Path            string
	Version         string
	Headers         [][2]string
	RawHeaders      [][2][]byte
	Host            string
	ContentLength   int64
	ChunkedEncoding bool
	KeepAlive       bool
	HeadersComplete bool
	BodyRead        int64
	ExpectContinue  bool

	// UpgradeH2C is true iff this request is a valid h2c upgrade request
	// (RFC 7540 §3.2): Upgrade: h2c (single token, exclusively) + HTTP2-Settings
	// header present + Connection header listing BOTH "upgrade" and
	// "http2-settings" tokens. The single-token Upgrade requirement
	// disambiguates from the WebSocket path (Upgrade: websocket).
	UpgradeH2C bool
	// HTTP2Settings holds the raw base64url-encoded HTTP2-Settings value
	// (still encoded; caller decodes). Non-empty only when the header is present.
	HTTP2Settings string
}

// Reset clears all fields, reusing existing header slice capacity.
func (r *Request) Reset() {
	r.Method = ""
	r.Path = ""
	r.Version = ""
	r.Headers = r.Headers[:0]
	r.RawHeaders = r.RawHeaders[:0]
	r.Host = ""
	r.ContentLength = 0
	r.ChunkedEncoding = false
	r.KeepAlive = false
	r.HeadersComplete = false
	r.BodyRead = 0
	r.ExpectContinue = false
	r.UpgradeH2C = false
	r.HTTP2Settings = ""
}
