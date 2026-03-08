package h1

const MaxHeaderSize = 16 << 20 // 16MB

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
