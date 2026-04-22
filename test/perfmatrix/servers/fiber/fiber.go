// Package fiber registers the fiber v3 H1 server against the
// perfmatrix registry. fiber does not support HTTP/2, so only the "h1"
// cell-column is registered.
package fiber

// Wave-2 fills in init() with a single servers.Register for
// "fiber-h1".
func init() {}
