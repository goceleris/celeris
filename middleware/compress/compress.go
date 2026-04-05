package compress

import (
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	kgzip "github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/goceleris/celeris"
)

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
			c.AddHeader("vary", "Accept-Encoding")
			return c.Next()
		}

		encoding := c.AcceptsEncodings(encodings...)
		if encoding == "" {
			// No matching encoding. Still add Vary so caches know the
			// response varies by Accept-Encoding even when uncompressed.
			c.AddHeader("vary", "Accept-Encoding")
			return c.Next()
		}

		c.BufferResponse()
		err := c.Next()

		// All buffered paths below add Vary before flushing.

		status := c.ResponseStatus()
		body := c.ResponseBody()

		if status < 200 || status >= 300 || len(body) == 0 || len(body) < minLen {
			c.AddHeader("vary", "Accept-Encoding")
			if ferr := c.FlushResponse(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		// Skip if response already has content-encoding.
		for _, h := range c.ResponseHeaders() {
			if h[0] == "content-encoding" {
				c.AddHeader("vary", "Accept-Encoding")
				if ferr := c.FlushResponse(); ferr != nil && err == nil {
					err = ferr
				}
				return err
			}
		}

		ct := c.ResponseContentType()
		if isExcluded(ct, excluded) {
			c.AddHeader("vary", "Accept-Encoding")
			if ferr := c.FlushResponse(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		compressed, compErr := compressBody(encoding, body, gzipPool, brotliPool, zstdEncoder, &bufPool)
		if compErr != nil {
			c.AddHeader("vary", "Accept-Encoding")
			if ferr := c.FlushResponse(); ferr != nil && err == nil {
				err = ferr
			}
			if err == nil {
				err = compErr
			}
			return err
		}

		// Compression expansion guard.
		if len(compressed) >= len(body) {
			c.AddHeader("vary", "Accept-Encoding")
			if ferr := c.FlushResponse(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		c.SetResponseBody(compressed)
		c.SetHeader("content-encoding", encoding)
		c.AddHeader("vary", "Accept-Encoding")
		if ferr := c.FlushResponse(); ferr != nil && err == nil {
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

func compressZstd(body []byte, enc *zstd.Encoder) ([]byte, error) {
	return enc.EncodeAll(body, nil), nil
}
