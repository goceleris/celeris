package compress

import (
	"bytes"
	"compress/flate"
	"fmt"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	kgzip "github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/goceleris/celeris"
)

// addVary adds Vary: Accept-Encoding to c's response unless a Vary line
// already names it: Context.AcceptsEncodings adds one (celeris#912), and the
// handler may have too. The response then names it once.
func addVary(c *celeris.Context) {
	if !varyNamesAcceptEncoding(c.ResponseHeaders()) {
		c.AddHeader("vary", "Accept-Encoding")
	}
}

// varyNamesAcceptEncoding reports whether a Vary line in headers lists
// Accept-Encoding (in any case) or is "*".
func varyNamesAcceptEncoding(headers [][2]string) bool {
	for _, h := range headers {
		if len(h[0]) != 4 || !strings.EqualFold(h[0], "vary") {
			continue
		}
		for v := h[1]; v != ""; {
			tok := v
			if i := strings.IndexByte(v, ','); i >= 0 {
				tok, v = v[:i], v[i+1:]
			} else {
				v = ""
			}
			tok = strings.TrimSpace(tok)
			if tok == "*" || strings.EqualFold(tok, "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

// compressibleKey is the context key under which middleware/static and
// middleware/etag mark a response whose strong ETag is a hash of the identity
// bytes, which compress weakens when it encodes them (celeris#888 item 36).
// The value implements [compressibleRepresentation]. The key is part of no
// API: it is reserved for the middleware of this repository, and a response
// that nothing marked is judged by compress as before. The string is repeated
// in static and etag (this module cannot import an internal package of the
// core that older releases lack), and the three packages' tests pin it.
const compressibleKey = "celeris.compressible-representation"

// compressActiveKey is the context key compress sets (to true) when it has
// negotiated an encoding for the request and runs the handlers below it with
// the response buffered. static and etag mark a response (compressibleKey)
// only when it is set, so they pay for the mark only where a compress in front
// can use it (and the mark follows compress's own decisions: its Skip, HEAD,
// OPTIONS, streaming requests and "no encoding in common" never get one).
const compressActiveKey = "celeris.compress.active"

// compressibleRepresentation is what a marker (the value under
// compressibleKey) answers: the size of the identity representation and its
// content type, which are what compress decides on whether to encode it
// (MinLength, ExcludedContentTypes). A 304 has neither a body nor a content
// type, so the marker is how compress decides for it what it decided for the
// 200.
type compressibleRepresentation interface {
	CompressibleRepresentation() (size int, contentType string)
}

// marked returns the marker of the response, when a handler set one.
func marked(c *celeris.Context) (compressibleRepresentation, bool) {
	v, ok := c.Get(compressibleKey)
	if !ok {
		return nil, false
	}
	m, ok := v.(compressibleRepresentation)
	return m, ok
}

// weakenETag turns the strong ETag of c's response into a weak one: the tag
// was computed over the identity bytes, but the wire carries an encoded form
// of them (or, for a response marked as compressible, may: see
// weakenMarkedNotModified). RFC 9110 8.8.3 says a strong validator changes with
// every octet of the representation, Content-Encoding included.
func weakenETag(c *celeris.Context) {
	for _, h := range c.ResponseHeaders() {
		if h[0] == "etag" && len(h[1]) > 0 && h[1][0] != 'W' {
			c.SetHeader("etag", "W/"+h[1])
			return
		}
	}
}

// weakenMarkedNotModified gives the 304 the tag the 200 to the same request
// would carry (RFC 9110 15.4.5). It has no body to encode and no content type,
// so the marker of the handler that answered it stands in for them: a
// representation compress would encode (not under MinLength, not of an
// excluded content type, no Content-Encoding of its own) has its strong tag
// weakened, as it is on the 200. Whether the encoded form would be longer than
// the identity one cannot be known without encoding it: the 200 of a marked
// response compress does not send encoded for that reason carries the weak tag
// too (see weakenETag's caller), so the two agree.
func weakenMarkedNotModified(c *celeris.Context, excluded []string, minLen int) {
	strong := false
	for _, h := range c.ResponseHeaders() {
		switch h[0] {
		case "content-encoding":
			return
		case "etag":
			strong = len(h[1]) > 0 && h[1][0] != 'W'
		}
	}
	if !strong {
		return
	}
	m, ok := marked(c)
	if !ok {
		return
	}
	size, ct := m.CompressibleRepresentation()
	if size == 0 || (minLen > 0 && size < minLen) || isExcluded(ct, excluded) {
		return
	}
	weakenETag(c)
}

// New creates a compress middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	// Build pools at init.
	var gzipPool *sync.Pool
	var brotliPool *sync.Pool
	var deflatePool *sync.Pool
	var zstdEncoder *zstd.Encoder
	var bufPool sync.Pool

	bufPool.New = func() any {
		return new(bytes.Buffer)
	}

	for _, enc := range cfg.Encodings {
		switch enc {
		case "gzip":
			level := resolveGzipLevel(cfg.GzipLevel)
			gzipPool = &sync.Pool{
				New: func() any {
					w, _ := kgzip.NewWriterLevel(nil, level)
					return w
				},
			}
		case "br":
			level := resolveBrotliLevel(cfg.BrotliLevel)
			brotliPool = &sync.Pool{
				New: func() any {
					return brotli.NewWriterLevel(nil, level)
				},
			}
		case "zstd":
			level := resolveZstdLevel(cfg.ZstdLevel)
			var err error
			zstdEncoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
			if err != nil {
				panic("compress: zstd init: " + err.Error())
			}
		case "deflate":
			level := resolveDeflateLevel(cfg.DeflateLevel)
			deflatePool = &sync.Pool{
				New: func() any {
					w, _ := flate.NewWriter(nil, level)
					return w
				},
			}
		}
	}

	// Build excluded content-type prefixes (lowercased).
	excluded := make([]string, len(cfg.ExcludedContentTypes))
	for i, ct := range cfg.ExcludedContentTypes {
		excluded[i] = strings.ToLower(ct)
	}

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	encodings := cfg.Encodings
	minLen := cfg.MinLength

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		m := c.Method()
		if m == "HEAD" || m == "OPTIONS" {
			addVary(c)
			return c.Next()
		}

		// Streaming protocols are incompatible with response buffering.
		// SSE produces incremental events; WebSocket upgrades the
		// connection. Detect both before BufferResponse otherwise the
		// downstream handler's StreamWriter() returns nil (because
		// bufferDepth > 0) and the request fails with 500.
		if isStreamingRequest(c) {
			addVary(c)
			return c.Next()
		}

		encoding := c.AcceptsEncodings(encodings...)
		if encoding == "" {
			// No matching encoding. Vary still names Accept-Encoding so
			// caches know the response varies by it even when uncompressed.
			// AcceptsEncodings has named it (celeris#912); addVary keeps the
			// line with a core older than that and adds no second one.
			addVary(c)
			return c.Next()
		}

		c.Set(compressActiveKey, true)
		c.BufferResponse()
		err := c.Next()

		// flushWithVary makes sure Vary names Accept-Encoding (a handler can
		// have replaced the line AcceptsEncodings added) and flushes the
		// (possibly uncompressed) buffered response. Used by every
		// early-return and the final success path below.
		flushWithVary := func() error {
			addVary(c)
			return c.FlushResponse()
		}

		status := c.ResponseStatus()
		body := c.ResponseBody()

		// A 304 carries the tag a 200 to the same request would carry
		// (RFC 9110 15.4.5), and compress weakens the strong tag of the 200
		// it encodes: a handler that marked the response (static over an
		// fs.FS, etag) lets the 304 follow.
		if status == 304 {
			weakenMarkedNotModified(c, excluded, minLen)
			if ferr := flushWithVary(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		// A 206 goes out as it is: its Content-Range counts bytes of the
		// identity representation, and once a Content-Encoding applies a
		// range is over the encoded bytes (RFC 9110 §14.1.2), so a
		// compressed part could not be reassembled (celeris#832).
		if status < 200 || status >= 300 || status == 206 || len(body) == 0 || (minLen > 0 && len(body) < minLen) {
			if ferr := flushWithVary(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		// Skip if response already has content-encoding.
		for _, h := range c.ResponseHeaders() {
			if h[0] == "content-encoding" {
				if ferr := flushWithVary(); ferr != nil && err == nil {
					err = ferr
				}
				return err
			}
		}

		ct := c.ResponseContentType()
		if isExcluded(ct, excluded) {
			if ferr := flushWithVary(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		compressed, compErr := compressBody(encoding, body, gzipPool, brotliPool, deflatePool, zstdEncoder, &bufPool)
		if compErr != nil {
			// A marked response sent as it is keeps the weak tag its 304 gets
			// (see weakenMarkedNotModified): a weak tag on identity bytes is
			// legal, a strong tag that the 304 does not carry is not.
			if _, ok := marked(c); ok {
				weakenETag(c)
			}
			// Graceful degradation: if compression fails, flush the original
			// uncompressed body. The compression error is propagated to the caller
			// but the response is already sent to avoid a blank page.
			if ferr := flushWithVary(); ferr != nil && err == nil {
				err = ferr
			}
			if err == nil {
				err = compErr
			}
			return err
		}

		// Compression expansion guard.
		if len(compressed) >= len(body) {
			if _, ok := marked(c); ok {
				weakenETag(c)
			}
			if ferr := flushWithVary(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		c.SetResponseBody(compressed)
		c.SetHeader("content-encoding", encoding)
		// If a downstream handler or middleware (etag/static) set a strong
		// ETag, weakify it: that tag was computed over the uncompressed
		// body but the wire now carries the compressed form. RFC 7232 §2.3
		// says strong validators must match byte-for-byte, including
		// Content-Encoding — replaying the original tag against the
		// compressed payload would corrupt cache validation. The weak
		// "W/" prefix tells the client the tag describes payload semantics,
		// not octet identity.
		weakenETag(c)
		if ferr := flushWithVary(); ferr != nil && err == nil {
			err = ferr
		}
		return err
	}
}

// resolveGzipLevel maps sentinel levels to klauspost/gzip library values.
func resolveGzipLevel(l Level) int {
	switch l {
	case LevelDefault:
		return kgzip.DefaultCompression // -1
	case LevelNone:
		return kgzip.NoCompression // 0
	case LevelBest:
		return kgzip.BestCompression // 9
	default:
		return int(l)
	}
}

// resolveBrotliLevel maps sentinel levels to brotli library values.
func resolveBrotliLevel(l Level) int {
	switch l {
	case LevelDefault:
		return int(defaultBrotliLevel) // 6
	case LevelNone:
		return 0
	case LevelBest:
		return 11
	default:
		return int(l)
	}
}

// resolveZstdLevel maps sentinel levels to zstd library values.
func resolveZstdLevel(l Level) zstd.EncoderLevel {
	switch l {
	case LevelDefault:
		return zstd.SpeedDefault // 3
	case LevelNone:
		return zstd.SpeedFastest // 1 (zstd has no "store" mode; fastest is closest)
	case LevelBest:
		return zstd.SpeedBestCompression // 4
	case LevelFastest:
		return zstd.SpeedFastest // 1
	default:
		if l < Level(zstd.SpeedFastest) {
			return zstd.SpeedFastest
		}
		if l > Level(zstd.SpeedBestCompression) {
			return zstd.SpeedBestCompression
		}
		return zstd.EncoderLevel(l)
	}
}

func compressBody(
	encoding string,
	body []byte,
	gzipPool *sync.Pool,
	brotliPool *sync.Pool,
	deflatePool *sync.Pool,
	zstdEncoder *zstd.Encoder,
	bufPool *sync.Pool,
) ([]byte, error) {
	switch encoding {
	case "gzip":
		return compressGzip(body, gzipPool, bufPool)
	case "br":
		return compressBrotli(body, brotliPool, bufPool)
	case "zstd":
		return compressZstd(body, zstdEncoder)
	case "deflate":
		return compressDeflate(body, deflatePool, bufPool)
	default:
		return nil, fmt.Errorf("compress: unsupported encoding: %s", encoding)
	}
}

// maxPooledBufSize is the maximum buffer capacity retained in the pool.
// Buffers that grew beyond this (e.g., from large responses) are discarded
// to avoid holding excessive memory.
const maxPooledBufSize = 32 << 10 // 32KB

func compressGzip(body []byte, pool *sync.Pool, bufPool *sync.Pool) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBufSize {
			bufPool.Put(buf)
		}
	}()

	w := pool.Get().(*kgzip.Writer)
	w.Reset(buf)

	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		pool.Put(w)
		return nil, err
	}
	if err := w.Close(); err != nil {
		pool.Put(w)
		return nil, err
	}
	pool.Put(w)

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

func compressBrotli(body []byte, pool *sync.Pool, bufPool *sync.Pool) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBufSize {
			bufPool.Put(buf)
		}
	}()

	w := pool.Get().(*brotli.Writer)
	w.Reset(buf)

	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		pool.Put(w)
		return nil, err
	}
	if err := w.Close(); err != nil {
		pool.Put(w)
		return nil, err
	}
	pool.Put(w)

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

func compressDeflate(body []byte, pool *sync.Pool, bufPool *sync.Pool) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBufSize {
			bufPool.Put(buf)
		}
	}()

	w := pool.Get().(*flate.Writer)
	w.Reset(buf)

	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		pool.Put(w)
		return nil, err
	}
	if err := w.Close(); err != nil {
		pool.Put(w)
		return nil, err
	}
	pool.Put(w)

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

func compressZstd(body []byte, enc *zstd.Encoder) ([]byte, error) {
	return enc.EncodeAll(body, nil), nil
}

// isStreamingRequest detects requests whose responses must not be buffered
// by this middleware: SSE (Accept: text/event-stream) and WebSocket
// upgrades (Upgrade: websocket). Both rely on c.StreamWriter() / Hijack
// flows that fail when bufferDepth > 0.
func isStreamingRequest(c *celeris.Context) bool {
	if accept := c.Header("accept"); accept != "" {
		// Plain comparison rather than full media-type parsing — SSE
		// uses exactly "text/event-stream"; clients may add q-values
		// or list other types alongside it.
		if strings.Contains(accept, "text/event-stream") {
			return true
		}
	}
	if upg := c.Header("upgrade"); upg != "" {
		if strings.EqualFold(upg, "websocket") {
			return true
		}
	}
	return false
}
