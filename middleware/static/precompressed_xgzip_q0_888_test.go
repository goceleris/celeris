package static

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// celeris#888: a client that refuses gzip by name ("gzip;q=0, *") is not sent
// the .gz variant because "*" would reach x-gzip, which is gzip (RFC 9110
// 8.4.1.3): the wildcard stands only for what the header does not list.
func TestPreCompressedGzipQ0WildcardStaysIdentity888(t *testing.T) {
	rows := []struct {
		ae       string
		wantEnc  string
		wantBody string
	}{
		{"gzip;q=0, br;q=0, *", "", plain888},
		{"gzip;q=0, *;q=0.5, br;q=0", "", plain888},
		{"x-gzip;q=0, br;q=0, *", "", plain888},
		{"gzip;q=0, *", "br", br888},
		{"br;q=0, *", "gzip", gz888},
		{"br;q=0, x-gzip", "gzip", gz888},
	}
	for name, mw := range q0Fixture888(t) {
		for _, row := range rows {
			t.Run(name+"/"+row.ae, func(t *testing.T) {
				ctx, rec := celeristest.NewContext("GET", "/a.js",
					celeristest.WithHeader("accept-encoding", row.ae), celeristest.WithHandlers(mw))
				defer celeristest.ReleaseContext(ctx)
				if err := ctx.Next(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ce := rec.Header("content-encoding"); ce != row.wantEnc {
					t.Errorf("content-encoding %q, want %q", ce, row.wantEnc)
				}
				if got := rec.BodyString(); got != row.wantBody {
					t.Errorf("body %q, want %q", got, row.wantBody)
				}
			})
		}
	}
}
