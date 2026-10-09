//go:build go1.27 && !http2legacy

package std

import "net/http"

// h2cBaseConfig is the BaseConfig for ServeConn: none under the go1.27
// wrapper, where a BaseConfig would serve the connection on a one-off
// http.Server that Shutdown cannot reach (celeris#878). The wrapper takes the
// timeouts and limits from the server the connection is served through
// (e.server), and IdleTimeout from the http2.Server registered with it.
func h2cBaseConfig(*http.Request) *http.Server { return nil }
