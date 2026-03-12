package celeris_test

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

func FuzzParseFormURLEncoded(f *testing.F) {
	f.Add("key=value")
	f.Add("a=1&b=2&c=3")
	f.Add("")
	f.Add("=")
	f.Add("key=val%20ue")
	f.Add("k=v&k=v2")
	f.Add("%zz=bad")
	f.Add("a" + string([]byte{0}) + "=b")
	f.Fuzz(func(_ *testing.T, body string) {
		ctx, _ := celeristest.NewContext("POST", "/form",
			celeristest.WithBody([]byte(body)),
			celeristest.WithContentType("application/x-www-form-urlencoded"),
		)
		defer celeristest.ReleaseContext(ctx)
		_ = ctx.FormValue("key") // must not panic
	})
}

func FuzzCookieParsing(f *testing.F) {
	f.Add("session=abc123")
	f.Add("a=1; b=2; c=3")
	f.Add("")
	f.Add("=")
	f.Add("no_equals")
	f.Add("key=val=ue")
	f.Add("; ; ;")
	f.Add("a=b;c=d;e=f")
	f.Fuzz(func(_ *testing.T, cookieHeader string) {
		ctx, _ := celeristest.NewContext("GET", "/",
			celeristest.WithHeader("cookie", cookieHeader),
		)
		defer celeristest.ReleaseContext(ctx)
		_, _ = ctx.Cookie("session") // must not panic
	})
}
