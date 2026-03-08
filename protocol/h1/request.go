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
}
