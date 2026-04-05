package etag_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/etag"
)

func ExampleNew() {
	// Weak ETags (default) -- suitable for responses that may be
	// content-negotiated or transfer-encoded.
	// s := celeris.New()
	// s.Use(etag.New())
	_ = etag.New()
}

func ExampleNew_strong() {
	// Strong ETags -- byte-for-byte identical guarantee.
	_ = etag.New(etag.Config{Strong: true})
}

func ExampleNew_skip() {
	// Dynamically skip ETag for server-sent event streams.
	_ = etag.New(etag.Config{
		Skip: func(c *celeris.Context) bool {
			return strings.HasPrefix(c.Path(), "/events")
		},
	})
}

func ExampleNew_skipPaths() {
	// Skip ETag computation for large-payload endpoints.
	_ = etag.New(etag.Config{
		SkipPaths: []string{"/download", "/export", "/stream"},
	})
}

func ExampleNew_hashFunc() {
	// Use SHA-256 (truncated) instead of the default CRC-32.
	_ = etag.New(etag.Config{
		HashFunc: func(body []byte) string {
			h := sha256.Sum256(body)
			return hex.EncodeToString(h[:16])
		},
	})
}
