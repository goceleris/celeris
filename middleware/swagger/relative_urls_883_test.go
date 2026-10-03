package swagger

import (
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// celeris#883: with the default config the page's spec URL and the
// {BasePath} redirect must work behind a reverse proxy that publishes the
// app under another prefix and strips it (the browser asks for
// /ext/swagger/, the app sees /swagger/; the proxy forwards only /ext/...).

// pageSpecURL883 returns the spec URL the page hands its renderer, as the
// browser reads it.
func pageSpecURL883(t *testing.T, r UIRenderer, body string) string {
	t.Helper()
	if r == RendererScalar {
		m := dataURLRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("Scalar page has no data-url:\n%s", body)
		}
		return html.UnescapeString(m[1])
	}
	re := swaggerURLRe
	if r == RendererReDoc {
		re = redocInitRe
	}
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s page: no spec URL matched %s in:\n%s", r, re, body)
	}
	var u string
	if err := json.Unmarshal([]byte(m[1]), &u); err != nil {
		t.Fatalf("spec URL %s is not a JS string literal: %v", m[1], err)
	}
	return u
}

// TestDefaultSpecURLBehindPrefixStrippingProxy883: for every renderer and
// BasePath, the default page's spec URL, resolved against the page's URL,
// names {BasePath}/spec when the page is loaded directly and
// /ext{BasePath}/spec when it is loaded through the proxy; the middleware
// answers both with the spec.
func TestDefaultSpecURLBehindPrefixStrippingProxy883(t *testing.T) {
	t.Parallel()
	for _, r := range []UIRenderer{RendererSwaggerUI, RendererScalar, RendererReDoc} {
		for _, bp := range []string{"", "/docs/api/", "/"} {
			t.Run(string(r)+"/basepath="+bp, func(t *testing.T) {
				t.Parallel()
				trimmed := strings.TrimRight(bp, "/")
				if bp == "" {
					trimmed = "/swagger"
				}
				cfg := Config{SpecContent: jsonSpec, BasePath: bp, Renderer: r, CDN: r != RendererSwaggerUI}
				mw := New(cfg)
				ref := pageSpecURL883(t, r, servePageAt425(t, cfg, trimmed+"/"))
				for _, public := range []struct{ page, strip string }{
					{"https://app.test" + trimmed + "/", ""},
					{"https://proxy.test/ext" + trimmed + "/", "/ext"},
				} {
					u := resolve425(t, public.page, ref)
					page := resolve425(t, public.page, "")
					if u.Host != page.Host || u.Path != public.strip+trimmed+"/spec" {
						t.Fatalf("page at %s: spec URL %q resolves to %s, want %s%s/spec on the same origin", public.page, ref, u, public.strip, trimmed)
					}
					rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", strings.TrimPrefix(u.Path, public.strip))
					testutil.AssertNoError(t, err)
					testutil.AssertStatus(t, rec, 200)
					if rec.BodyString() != string(jsonSpec) {
						t.Fatalf("GET %s: body %.60q, want the spec", u.Path, rec.BodyString())
					}
				}
			})
		}
	}
}

// TestBasePathRedirectBehindPrefixStrippingProxy883: the {BasePath} redirect's
// Location, resolved against the request URL, is the page both directly and
// through the proxy, for nested BasePaths and a last segment with a colon
// (which an unprefixed relative reference would read as a URI scheme).
func TestBasePathRedirectBehindPrefixStrippingProxy883(t *testing.T) {
	t.Parallel()
	for _, bp := range []string{"", "/docs/api", "/v1/a:b/"} {
		t.Run("basepath="+bp, func(t *testing.T) {
			t.Parallel()
			trimmed := strings.TrimRight(bp, "/")
			if bp == "" {
				trimmed = "/swagger"
			}
			mw := New(Config{SpecContent: jsonSpec, BasePath: bp})
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", trimmed)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 301)
			loc := rec.Header("location")
			for _, public := range []struct{ req, strip string }{
				{"https://app.test" + trimmed, ""},
				{"https://proxy.test/ext" + trimmed, "/ext"},
			} {
				u := resolve425(t, public.req, loc)
				req := resolve425(t, public.req, "")
				if u.Scheme != req.Scheme || u.Host != req.Host || u.Path != public.strip+trimmed+"/" || u.RawQuery != "" {
					t.Fatalf("GET %s: Location %q resolves to %s, want %s%s/ on the same origin", public.req, loc, u, public.strip, trimmed)
				}
				rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", strings.TrimPrefix(u.Path, public.strip))
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				testutil.AssertHeaderContains(t, rec, "content-type", "text/html")
			}
		})
	}
}
