package etag

import (
	"hash/crc32"

	"github.com/goceleris/celeris"
)

// New creates an ETag middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	strong := cfg.Strong
	hashFunc := cfg.HashFunc

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		m := c.Method()
		if m != "GET" && m != "HEAD" {
			return c.Next()
		}

		c.BufferResponse()
		err := c.Next()

		status := c.ResponseStatus()
		body := c.ResponseBody()

		if status < 200 || status >= 300 || len(body) == 0 {
			if ferr := c.FlushResponse(); ferr != nil && err == nil {
				err = ferr
			}
			return err
		}

		// Check if handler already set an etag header.
		existingTag := ""
		for _, h := range c.ResponseHeaders() {
			if h[0] == "etag" {
				existingTag = h[1]
				break
			}
		}

		var tag string
		if existingTag != "" {
			tag = existingTag
		} else if hashFunc != nil {
			opaque := hashFunc(body)
			tag = formatETag(opaque, !strong)
		} else {
			// Inline CRC-32 ETag formatting: the [14]byte buffer lives on
			// the closure's stack frame, avoiding the heap escape that a
			// returned-string helper would cause.
			checksum := crc32.ChecksumIEEE(body)
			var tagBuf [14]byte
			n := writeCRC32ETag(&tagBuf, checksum, !strong)
			tag = string(tagBuf[:n])
		}

		inm := c.Header("if-none-match")
		if inm != "" && etagMatch(inm, tag) {
			if existingTag == "" {
				c.SetHeader("etag", tag)
			}
			c.DiscardBufferedResponse()
			if nerr := c.NoContent(304); nerr != nil && err == nil {
				err = nerr
			}
			return err
		}

		if existingTag == "" {
			c.SetHeader("etag", tag)
		}
		if ferr := c.FlushResponse(); ferr != nil && err == nil {
			err = ferr
		}
		return err
	}
}

// hexDigit returns the lowercase hex character for the low 4 bits of b.
func hexDigit(b byte) byte {
	b &= 0x0F
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// writeCRC32ETag writes a CRC-32 ETag into dst and returns the number of
// bytes written. The caller owns the buffer so it stays on the caller's
// stack, avoiding the heap escape that a returned-string helper would cause.
func writeCRC32ETag(dst *[14]byte, checksum uint32, weak bool) int {
	off := 0
	if weak {
		dst[0] = 'W'
		dst[1] = '/'
		dst[2] = '"'
		off = 3
	} else {
		dst[0] = '"'
		off = 1
	}
	dst[off] = hexDigit(byte(checksum >> 28))
	dst[off+1] = hexDigit(byte(checksum >> 24))
	dst[off+2] = hexDigit(byte(checksum >> 20))
	dst[off+3] = hexDigit(byte(checksum >> 16))
	dst[off+4] = hexDigit(byte(checksum >> 12))
	dst[off+5] = hexDigit(byte(checksum >> 8))
	dst[off+6] = hexDigit(byte(checksum >> 4))
	dst[off+7] = hexDigit(byte(checksum))
	dst[off+8] = '"'
	return off + 9
}

// formatETag formats an opaque-tag string as an ETag value.
func formatETag(opaque string, weak bool) string {
	if weak {
		return `W/"` + opaque + `"`
	}
	return `"` + opaque + `"`
}

// opaqueTag strips the W/ prefix and quotes from an ETag value.
// Returns the opaque-tag for weak comparison per RFC 7232 Section 2.3.2.
func opaqueTag(etag string) string {
	if len(etag) >= 4 && etag[0] == 'W' && etag[1] == '/' {
		etag = etag[2:]
	}
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag[1 : len(etag)-1]
	}
	return etag
}

// etagMatch checks if the If-None-Match header matches the given tag.
// Implements weak comparison per RFC 7232 Section 2.3.2.
func etagMatch(inm, tag string) bool {
	tagOpaque := opaqueTag(tag)

	if inm == "*" {
		return true
	}

	for len(inm) > 0 {
		// Skip whitespace.
		for len(inm) > 0 && inm[0] == ' ' {
			inm = inm[1:]
		}
		if len(inm) == 0 {
			break
		}

		var candidate string
		if inm[0] == '"' {
			end := 1
			for end < len(inm) && inm[end] != '"' {
				end++
			}
			if end < len(inm) {
				candidate = inm[:end+1]
				inm = inm[end+1:]
			} else {
				candidate = inm
				inm = ""
			}
		} else if len(inm) >= 2 && inm[0] == 'W' && inm[1] == '/' {
			if len(inm) >= 3 && inm[2] == '"' {
				end := 3
				for end < len(inm) && inm[end] != '"' {
					end++
				}
				if end < len(inm) {
					candidate = inm[:end+1]
					inm = inm[end+1:]
				} else {
					candidate = inm
					inm = ""
				}
			} else {
				break
			}
		} else {
			end := 0
			for end < len(inm) && inm[end] != ',' {
				end++
			}
			candidate = inm[:end]
			inm = inm[end:]
		}

		if opaqueTag(candidate) == tagOpaque {
			return true
		}

		// Skip comma and whitespace.
		for len(inm) > 0 && (inm[0] == ',' || inm[0] == ' ') {
			inm = inm[1:]
		}
	}

	return false
}
