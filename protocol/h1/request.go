package h1

// MaxHeaderSize is the maximum allowed size of HTTP headers in bytes.
const MaxHeaderSize = 16 << 20 // 16MB

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
