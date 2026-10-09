//go:build !go1.27 || http2legacy

package std

import "net/http"

// h2cBaseConfig is the BaseConfig for ServeConn: the server the request came
// in on. x/net's own HTTP/2 server (before go1.27, or built with
// -tags http2legacy) takes ReadTimeout, WriteTimeout, MaxHeaderBytes,
// ConnState and ErrorLog from it, and with none falls back to a zero
// http.Server, which would drop all of them from an h2c connection. The
// GOAWAY at the start of the drain does not depend on it: that is the
// registration http2.ConfigureServer made with the server.
func h2cBaseConfig(r *http.Request) *http.Server {
	s, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
	return s
}
