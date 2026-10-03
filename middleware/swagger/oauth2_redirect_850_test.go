package swagger

import (
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// celeris#850: Swagger UI's OAuth2 flows send the browser back to an
// oauth2-redirect.html page that must share the UI page's origin (it calls
// window.opener.swaggerUIRedirectOauth2). The middleware must serve it where
// Swagger UI looks for it by default.

// upstreamOAuth2SRI850 is the sha384 of each file as published in the
// swagger-ui-dist 5.33.1 npm tarball (checked against the registry's sha512
// integrity), written out here so that a changed embedded byte fails.
var upstreamOAuth2SRI850 = map[string]string{
	"oauth2-redirect.html": "sha384-za/qRiugmILtAppLWMfMWGyvsEBU88bQlNzPRQAhLcvvE62Kp/Dk1eMVU74EFCTK",
	"oauth2-redirect.js":   "sha384-XuY48ztmqRBrZqX+bDrPUqkTumNohu9Bl+yztOEp/hDTS5qXIApmbiD04MrTFpjk",
}

var scriptSrcRe850 = regexp.MustCompile(`<script src="([^"]+)"></script>`)

// nextHandler850 answers whatever the middleware passes on, so a path the
// middleware does not serve is told apart from one it does. The chain runs
// it only for a path the middleware passes on: it must not follow a path the
// middleware answers, as Next would run it after the answer too.
func nextHandler850(c *celeris.Context) error {
	return c.String(404, "next handler")
}

// swaggerUIDefaultRedirect850 is the redirect URL Swagger UI 5 computes when
// oauth2RedirectUrl is unset, for a page at pagePath (swagger-ui-bundle.js:
// `globalThis.location.pathname.substring(0,globalThis.location.pathname.
// lastIndexOf("/"))}/oauth2-redirect.html`, asserted by
// TestOAuth2RedirectDefaultIsThePageDirectory850).
func swaggerUIDefaultRedirect850(pagePath string) string {
	return pagePath[:strings.LastIndex(pagePath, "/")] + "/oauth2-redirect.html"
}

// getFile850 GETs p through the middleware and checks that it answered with
// the upstream bytes of name, its content type and no cache lifetime.
func getFile850(t *testing.T, mw celeris.HandlerFunc, p, name, contentType string) string {
	t.Helper()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", p)
	testutil.AssertNoError(t, err)
	if rec.StatusCode != 200 || len(rec.Body) == 0 {
		t.Fatalf("GET %s: status %d, %d body bytes, want the middleware to serve %s", p, rec.StatusCode, len(rec.Body), name)
	}
	testutil.AssertHeader(t, rec, "content-type", contentType)
	testutil.AssertNoHeader(t, rec, "cache-control")
	if got, want := sri425(rec.Body), upstreamOAuth2SRI850[name]; got != want {
		t.Fatalf("GET %s: body %s (%d bytes), want the upstream %s %s", p, got, len(rec.Body), name, want)
	}
	return rec.BodyString()
}

// TestOAuth2RedirectPageServed850 follows what a browser does: from the page
// URL it derives Swagger UI's default redirect URL, loads that page, then the
// script the page references. Both must be the upstream files, for every
// asset source and BasePath, loaded directly and through a reverse proxy
// that strips a path prefix.
func TestOAuth2RedirectPageServed850(t *testing.T) {
	t.Parallel()
	for _, v := range swaggerUIAssetsVariants {
		for _, bp := range []string{"", "/docs/api/", "/"} {
			t.Run(v.name+"/basepath="+bp, func(t *testing.T) {
				t.Parallel()
				trimmed := strings.TrimRight(bp, "/")
				if bp == "" {
					trimmed = "/swagger"
				}
				mw := New(Config{SpecContent: jsonSpec, BasePath: bp, AssetsPath: v.assetsPath, CDN: v.cdn})
				for _, public := range []struct{ page, strip string }{
					{"https://app.test" + trimmed + "/", ""},
					{"https://proxy.test/ext" + trimmed + "/", "/ext"},
				} {
					pageURL := resolve425(t, public.page, "")
					redirect := resolve425(t, public.page, swaggerUIDefaultRedirect850(pageURL.Path))
					if redirect.Host != pageURL.Host || !strings.HasPrefix(redirect.Path, public.strip+"/") {
						t.Fatalf("redirect URL %s leaves the page's origin or prefix", redirect)
					}
					internal := strings.TrimPrefix(redirect.Path, public.strip)
					page := getFile850(t, mw, internal, "oauth2-redirect.html", "text/html; charset=utf-8")

					m := scriptSrcRe850.FindStringSubmatch(page)
					if m == nil {
						t.Fatalf("redirect page has no script reference: %q", page)
					}
					script := resolve425(t, redirect.String(), m[1])
					if script.Host != pageURL.Host || !strings.HasPrefix(script.Path, public.strip+"/") {
						t.Fatalf("redirect page script %q resolves to %s, off the page's origin or prefix", m[1], script)
					}
					body := getFile850(t, mw, strings.TrimPrefix(script.Path, public.strip), "oauth2-redirect.js", "text/javascript; charset=utf-8")
					if !strings.Contains(body, "window.opener.swaggerUIRedirectOauth2") {
						t.Fatalf("redirect script does not hand the result to window.opener.swaggerUIRedirectOauth2")
					}
				}

				p := trimmed + "/oauth2-redirect.html"
				rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", p)
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				_, err = testutil.RunMiddlewareWithMethod(t, mw, "POST", p)
				testutil.AssertHTTPError(t, err, 405)
			})
		}
	}
}

// TestOAuth2RedirectDefaultIsThePageDirectory850: the page leaves
// oauth2RedirectUrl unset by default, so the embedded bundle's runtime
// default applies, and that default is the formula the test above uses.
func TestOAuth2RedirectDefaultIsThePageDirectory850(t *testing.T) {
	t.Parallel()
	const runtimeDefault = "s.oauth2RedirectUrl=`${globalThis.location.protocol}//${globalThis.location.host}" +
		"${globalThis.location.pathname.substring(0,globalThis.location.pathname.lastIndexOf(\"/\"))}/oauth2-redirect.html`"
	if n := strings.Count(string(swaggerUIBundle), runtimeDefault); n != 1 {
		t.Fatalf("swagger-ui-bundle.js: the runtime oauth2RedirectUrl default occurs %d times, want 1", n)
	}
	for _, v := range swaggerUIAssetsVariants {
		if body := servePage(t, Config{SpecContent: jsonSpec, AssetsPath: v.assetsPath, CDN: v.cdn}); strings.Contains(body, "oauth2RedirectUrl") {
			t.Errorf("%s: the default page sets oauth2RedirectUrl, overriding Swagger UI's same-origin default", v.name)
		}
	}
}

// TestOAuth2RedirectPageOnlyForSwaggerUI850: Scalar and ReDoc do not use
// Swagger UI's redirect page, so its paths reach the next handler.
func TestOAuth2RedirectPageOnlyForSwaggerUI850(t *testing.T) {
	t.Parallel()
	for _, r := range []UIRenderer{RendererScalar, RendererReDoc} {
		mw := New(Config{SpecContent: jsonSpec, Renderer: r, CDN: true})
		for _, p := range []string{"/swagger/oauth2-redirect.html", "/swagger/oauth2-redirect.js"} {
			rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, nextHandler850}, "GET", p)
			testutil.AssertNoError(t, err)
			if rec.BodyString() != "next handler" {
				t.Errorf("%s: GET %s answered by the middleware", r, p)
			}
		}
	}
}
