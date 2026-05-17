package conn

import (
	"bytes"
	"strings"
	"testing"
)

// TestH1ResponseAdapter_EmptyBodyContentLength locks in RFC 9112 §6.2
// framing for bodiless responses on HTTP/1.1 keep-alive connections.
//
// Pre-fix bug discovered via the probatorium nightly run 25988957185:
// the h1ResponseAdapter (used by iouring + epoll engines) omitted
// Content-Length: 0 on every empty-body response — including the 301
// redirects c.Redirect emits and the 4xx/5xx error pages handlers
// return without a body. Keep-alive clients (Go net/http walker)
// received the headers + final CRLF, then waited for body bytes
// forever — exactly the 28k→1.5 req/s collapse the validator saw on
// static_swagger_proxy iouring/epoll cells the moment the chain
// stepped through /docs (which redirects to /docs/).
//
// std-engine bridge delegated to net/http which framed empty bodies
// via Transfer-Encoding: chunked + "0\r\n\r\n" terminator, masking
// the bug for std cells.
func TestH1ResponseAdapter_EmptyBodyContentLength(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		headers      [][2]string
		body         []byte
		wantCL       string // "" = must NOT appear
		wantCLAbsent bool   // explicitly assert no Content-Length (for 1xx/204)
	}{
		{
			name:    "301_redirect_no_body",
			status:  301,
			headers: [][2]string{{"location", "/target"}},
			body:    nil,
			wantCL:  "content-length: 0\r\n",
		},
		{
			name:    "302_redirect_no_body",
			status:  302,
			headers: [][2]string{{"location", "/target"}},
			body:    nil,
			wantCL:  "content-length: 0\r\n",
		},
		{
			name:    "400_no_body",
			status:  400,
			headers: nil,
			body:    nil,
			wantCL:  "content-length: 0\r\n",
		},
		{
			name:    "500_no_body",
			status:  500,
			headers: nil,
			body:    nil,
			wantCL:  "content-length: 0\r\n",
		},
		{
			name:    "200_no_body_via_NoContent",
			status:  200,
			headers: nil,
			body:    nil,
			wantCL:  "content-length: 0\r\n",
		},
		{
			name:         "204_no_content_omits_content_length",
			status:       204,
			headers:      nil,
			body:         nil,
			wantCLAbsent: true,
		},
		{
			name:         "304_not_modified_omits_content_length",
			status:       304,
			headers:      [][2]string{{"etag", "\"v1\""}},
			body:         nil,
			wantCLAbsent: true,
		},
		{
			name:         "100_continue_omits_content_length",
			status:       100,
			headers:      nil,
			body:         nil,
			wantCLAbsent: true,
		},
		{
			name:    "200_with_body_keeps_content_length",
			status:  200,
			headers: [][2]string{{"content-type", "application/json"}},
			body:    []byte(`{"ok":1}`),
			wantCL:  "content-length: 8\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured bytes.Buffer
			adapter := &h1ResponseAdapter{
				write:     func(p []byte) { captured.Write(p) },
				keepAlive: true,
			}
			if err := adapter.WriteResponse(nil, tc.status, tc.headers, tc.body); err != nil {
				t.Fatalf("WriteResponse: %v", err)
			}
			out := captured.String()
			// Case-insensitive header check: the adapter emits lowercase
			// names; the constants in this file use lowercase too.
			lower := strings.ToLower(out)
			if tc.wantCLAbsent {
				if strings.Contains(lower, "content-length:") {
					t.Errorf("unexpected content-length present:\n%s", out)
				}
				return
			}
			if !strings.Contains(lower, tc.wantCL) {
				t.Errorf("missing %q in response:\n%s", tc.wantCL, out)
			}
		})
	}
}
