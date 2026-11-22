package swagger

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// TestMethodNotAllowedNamesAllow888: a 405 MUST generate an Allow header
// (RFC 9110 15.5.6). The documentation paths, the spec and the embedded files
// answer GET and HEAD only.
func TestMethodNotAllowedNamesAllow888(t *testing.T) {
	mw := New(Config{SpecContent: jsonSpec})
	paths := []string{"/swagger/", "/swagger", "/swagger/spec", embeddedBundleURL425(t)}
	for _, p := range paths {
		for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
			t.Run(m+" "+p, func(t *testing.T) {
				ctx, _ := celeristest.NewContext(m, p, celeristest.WithHandlers(mw))
				defer celeristest.ReleaseContext(ctx)
				err := ctx.Next()
				testutil.AssertHTTPError(t, err, 405)
				var got string
				for _, h := range ctx.ResponseHeaders() {
					if h[0] == "allow" {
						got = h[1]
					}
				}
				if got != "GET, HEAD" {
					t.Errorf("405 for %s %s: Allow %q, want %q", m, p, got, "GET, HEAD")
				}
			})
		}
	}
	// GET and HEAD are not refused.
	for _, m := range []string{"GET", "HEAD"} {
		ctx, rec := celeristest.NewContext(m, "/swagger/spec", celeristest.WithHandlers(mw, func(*celeris.Context) error { return nil }))
		if err := ctx.Next(); err != nil || rec.StatusCode != 200 {
			t.Errorf("%s /swagger/spec: %v %d, want 200", m, err, rec.StatusCode)
		}
		celeristest.ReleaseContext(ctx)
	}
}
