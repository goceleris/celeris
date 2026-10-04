package celeris

import "testing"

// TestContextSchemeIsHTTPOrHTTPS924: celeris#924's family. Scheme returned
// the request's :scheme pseudo-header as the client sent it. On epoll and
// io_uring an h2c client chooses that value freely (the protocol layer checks
// only that it is present and not repeated), so a middleware that keys on
// Scheme (otel's url.scheme metric attribute) or builds a URL from it
// (redirect, rewrite) took whatever the client made up. Scheme now returns
// "https" when the pseudo-header says https, in any case, and "http"
// otherwise, as its godoc always said. An override set with SetScheme is
// returned as set.
func TestContextSchemeIsHTTPOrHTTPS924(t *testing.T) {
	for _, tc := range []struct {
		pseudo string // "" removes the :scheme pseudo-header
		want   string
	}{
		{"http", "http"},
		{"https", "https"},
		{"HTTPS", "https"},
		{"Http", "http"},
		{"x-made-up-1", "http"},
		{"javascript", "http"},
		{"https\x00", "http"},
		{"", "http"},
	} {
		s, _ := newTestStream("GET", "/test")
		kept := s.Headers[:0]
		for _, h := range s.Headers {
			if h[0] == ":scheme" {
				if tc.pseudo == "" {
					continue
				}
				h[1] = tc.pseudo
			}
			kept = append(kept, h)
		}
		s.Headers = kept
		c := acquireContext(s)
		if got := c.Scheme(); got != tc.want {
			t.Errorf(":scheme %q: Scheme() = %q, want %q", tc.pseudo, got, tc.want)
		}
		if got, want := c.IsTLS(), tc.want == "https"; got != want {
			t.Errorf(":scheme %q: IsTLS() = %v, want %v", tc.pseudo, got, want)
		}
		releaseContext(c)
		s.Release()
	}

	// An override is the server's own choice, returned as set.
	s, _ := newTestStream("GET", "/test")
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)
	c.SetScheme("https")
	if got := c.Scheme(); got != "https" {
		t.Errorf("SetScheme(https): Scheme() = %q", got)
	}
}
