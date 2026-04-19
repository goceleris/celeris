// Package jwtmccache provides a convenience [store.KV] constructor
// for JWT middleware's JWKSCache option backed by memcached.
//
// Mirror of [middleware/jwt/jwtcache] but built on
// [middleware/session/memcachedstore]. Either backend is valid for
// Config.JWKSCache — the middleware only needs Get/Set/Delete
// semantics; no extensions.
package jwtmccache

import (
	celmc "github.com/goceleris/celeris/driver/memcached"
	sessionmc "github.com/goceleris/celeris/middleware/session/memcachedstore"
	"github.com/goceleris/celeris/middleware/store"
)

// Options configure the JWKS memcached cache.
type Options struct {
	// KeyPrefix is prepended to every JWKS cache key. Default: "jwks:".
	KeyPrefix string
}

// New returns a [store.KV] suitable for use as JWT Config.JWKSCache.
// Internally reuses [session/memcachedstore.Store] with a JWKS-specific
// prefix.
func New(client *celmc.Client, opts ...Options) store.KV {
	prefix := "jwks:"
	if len(opts) > 0 && opts[0].KeyPrefix != "" {
		prefix = opts[0].KeyPrefix
	}
	return sessionmc.New(client, sessionmc.Options{KeyPrefix: prefix})
}
