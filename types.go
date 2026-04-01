package celeris

import "time"

// HandlerFunc defines the handler used by middleware and routes.
// Returning a non-nil error propagates it up through the middleware chain.
// The routerAdapter safety net writes an appropriate response for unhandled errors.
type HandlerFunc func(*Context) error

// DefaultMaxFormSize is the default maximum memory used for multipart form
// parsing (32 MB), matching net/http.
const DefaultMaxFormSize int64 = 32 << 20

// maxBodySize is the maximum request/response body size (100 MB), shared by
// stream responses (File, Stream), bridge adapter output, and ToHandler input.
const maxBodySize = 100 << 20

const maxStreamBodySize = maxBodySize

// SameSite controls the SameSite attribute of a cookie.
type SameSite int

const (
	// SameSiteDefaultMode leaves the SameSite attribute unset (browser default).
	SameSiteDefaultMode SameSite = iota
	// SameSiteLaxMode sets SameSite=Lax (cookies sent with top-level navigations).
	SameSiteLaxMode
	// SameSiteStrictMode sets SameSite=Strict (cookies sent only in first-party context).
	SameSiteStrictMode
	// SameSiteNoneMode sets SameSite=None (requires Secure; cookies sent in all contexts).
	SameSiteNoneMode
)

// String returns the SameSite attribute value ("Lax", "Strict", "None", or "").
func (s SameSite) String() string {
	switch s {
	case SameSiteLaxMode:
		return "Lax"
	case SameSiteStrictMode:
		return "Strict"
	case SameSiteNoneMode:
		return "None"
	default:
		return ""
	}
}

// Cookie represents an HTTP cookie for use with Context.SetCookie.
type Cookie struct {
	// Name is the cookie name.
	Name string
	// Value is the cookie value.
	Value string
	// Path limits the scope of the cookie to the given URL path.
	Path string
	// Domain limits the scope of the cookie to the given domain.
	Domain string
	// MaxAge=0 means no Max-Age attribute is sent. Negative value means
	// delete the cookie (Max-Age=0 in the header).
	MaxAge int
	// Expires sets the Expires attribute for legacy client compatibility
	// (e.g. IE11). Zero value means no Expires is sent. Prefer MaxAge
	// for modern clients.
	Expires time.Time
	// Secure flags the cookie for HTTPS-only transmission.
	Secure bool
	// HTTPOnly prevents client-side scripts from accessing the cookie.
	HTTPOnly bool
	// SameSite controls cross-site request cookie behavior.
	SameSite SameSite
}

// Param is a single URL parameter consisting of a key and a value.
type Param struct {
	// Key is the parameter name from the route pattern (e.g. "id" from ":id").
	Key string
	// Value is the matched segment from the request path (e.g. "42").
	Value string
}

// Params is a slice of Param.
type Params []Param

// Get returns the value of the first Param matching the given key.
// Returns empty string and false if the key is not found.
func (ps Params) Get(key string) (string, bool) {
	for _, p := range ps {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}
