package swagger

import (
	"crypto/sha512"
	"encoding/base64"
	"html"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// celeris#425: by default the page must load Swagger UI from files this
// middleware serves itself, not from a public CDN.

// upstreamSRI425 holds the sha384 of each file as published in the npm
// tarballs swagger-ui-dist 5.33.1, @scalar/api-reference 1.72.4 and redoc
// 2.5.4 (each tarball checked against the registry's sha512 integrity),
// keyed by package@version/path. It is written out here rather than read
// from pins.go so that a wrong pin or a changed embedded byte fails.
var upstreamSRI425 = map[string]string{
	"swagger-ui-dist@5.33.1/swagger-ui.css":                   "sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW",
	"swagger-ui-dist@5.33.1/swagger-ui-bundle.js":             "sha384-ZPehFMQommnnuaZ4rpxgkgTT2DKFVp4hZC/7pLit+9Lek9T1YGSo23eHFbvNkXkw",
	"swagger-ui-dist@5.33.1/swagger-ui-standalone-preset.js":  "sha384-My2aDM4r2Mbm3ybHcubKm9O9U8FEjvF/O5nGvE9YK5dzqOTbWEKa79RPJ1krdMaF",
	"@scalar/api-reference@1.72.4/dist/browser/standalone.js": "sha384-omTRdD9MbjA1vm12DqRUVvqJlr3VzSixvAdF1Jruu9AJOiJKyTKraIB6DyX+m10M",
	"redoc@2.5.4/bundles/redoc.standalone.js":                 "sha384-w447zOpYfw/1Tv/5AK9NfHTlQIqE3RVR6KY62jCyy9zNDgO64cMwGGP1Fj0zJVf5",
}

var (
	assetTagRe425  = regexp.MustCompile(`<(?:script|link)\b[^>]*>`)
	assetAttrRe425 = regexp.MustCompile(`\s(src|href|integrity|crossorigin)="([^"]*)"`)
)

// pageRef425 is one <script src> or <link href> of a served page, with its
// attributes HTML-unescaped as a browser reads them.
type pageRef425 struct {
	url, integrity, crossorigin string
	hasIntegrity                bool
}

// pageRefs425 returns every script and stylesheet reference in body.
func pageRefs425(body string) []pageRef425 {
	var refs []pageRef425
	for _, tag := range assetTagRe425.FindAllString(body, -1) {
		var r pageRef425
		for _, m := range assetAttrRe425.FindAllStringSubmatch(tag, -1) {
			v := html.UnescapeString(m[2])
			switch m[1] {
			case "src", "href":
				r.url = v
			case "integrity":
				r.integrity, r.hasIntegrity = v, true
			case "crossorigin":
				r.crossorigin = v
			}
		}
		if r.url != "" {
			refs = append(refs, r)
		}
	}
	return refs
}

func sri425(b []byte) string {
	sum := sha512.Sum384(b)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

func servePageAt425(t *testing.T, cfg Config, uiPath string) string {
	t.Helper()
	rec, err := testutil.RunMiddlewareWithMethod(t, New(cfg), "GET", uiPath)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "cache-control")
	return rec.BodyString()
}

// resolve425 resolves a page reference the way a browser does, against the
// URL the page was loaded from.
func resolve425(t *testing.T, pageURL, ref string) *url.URL {
	t.Helper()
	base, err := url.Parse(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		t.Fatalf("reference %q: %v", ref, err)
	}
	return base.ResolveReference(r)
}

// TestDefaultPageIsSelfContained425 loads the default Swagger UI page and
// fetches every file it references through the same middleware: each must
// resolve to a same-origin path under BasePath, answered with the exact
// upstream bytes, the right content type and an immutable cache lifetime.
// The page and the spec, whose URLs do not change on a redeploy, carry no
// cache lifetime at all.
func TestDefaultPageIsSelfContained425(t *testing.T) {
	t.Parallel()
	for _, bp := range []string{"", "/docs/api/", "/"} {
		t.Run("basepath="+bp, func(t *testing.T) {
			t.Parallel()
			trimmed := strings.TrimRight(bp, "/")
			if bp == "" {
				trimmed = "/swagger"
			}
			mw := New(Config{SpecContent: jsonSpec, BasePath: bp})
			body := servePageAt425(t, Config{SpecContent: jsonSpec, BasePath: bp}, trimmed+"/")
			for _, p := range []string{trimmed + "/", trimmed + "/spec"} {
				rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", p)
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				testutil.AssertNoHeader(t, rec, "cache-control")
			}

			refs := pageRefs425(body)
			if len(refs) != 3 {
				t.Fatalf("page has %d script/stylesheet references, want 3: %+v", len(refs), refs)
			}
			seen := map[string]bool{}
			for _, r := range refs {
				u := resolve425(t, "http://app.test"+trimmed+"/", r.url)
				if u.Host != "app.test" || !strings.HasPrefix(u.Path, trimmed+"/") {
					t.Errorf("reference %q resolves to %s, not to a same-origin path under %s/", r.url, u, trimmed)
					continue
				}
				name := path.Base(u.Path)
				want, ok := upstreamSRI425["swagger-ui-dist@5.33.1/"+name]
				if !ok {
					t.Errorf("reference %q is not a Swagger UI file", r.url)
					continue
				}
				seen[name] = true

				rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", u.Path)
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				if got := sri425(rec.Body); got != want {
					t.Errorf("GET %s: body %s (%d bytes), want the upstream file %s", u.Path, got, len(rec.Body), want)
				}
				wantType := "text/javascript"
				if strings.HasSuffix(name, ".css") {
					wantType = "text/css"
				}
				testutil.AssertHeaderContains(t, rec, "content-type", wantType)
				testutil.AssertHeaderContains(t, rec, "cache-control", "immutable")
			}
			if len(seen) != 3 {
				t.Errorf("page references %d distinct Swagger UI files, want 3 (css, bundle, preset): %v", len(seen), seen)
			}
		})
	}
}

// TestEmbeddedAssetsBehindPrefixStrippingProxy425: a reverse proxy that
// publishes the app under /ext/ and strips that prefix (the browser asks
// for /ext/swagger/, the app sees /swagger/) forwards only paths under
// /ext/. Every file the default page references, resolved against the
// page's public URL, must stay under /ext/ and, with /ext stripped, be
// answered by the middleware with the upstream bytes. Such a deployment
// sets a relative SpecURL, as here.
func TestEmbeddedAssetsBehindPrefixStrippingProxy425(t *testing.T) {
	t.Parallel()
	for _, bp := range []string{"/swagger", "/docs/api", "/"} {
		t.Run("basepath="+bp, func(t *testing.T) {
			t.Parallel()
			uiPath := strings.TrimRight(bp, "/") + "/"
			cfg := Config{SpecURL: "openapi.json", BasePath: bp}
			mw := New(cfg)
			public := "https://proxy.test/ext" + uiPath

			refs := pageRefs425(servePageAt425(t, cfg, uiPath))
			if len(refs) != 3 {
				t.Fatalf("page has %d script/stylesheet references, want 3: %+v", len(refs), refs)
			}
			for _, r := range refs {
				u := resolve425(t, public, r.url)
				if u.Host != "proxy.test" || !strings.HasPrefix(u.Path, "/ext/") {
					t.Errorf("page at %s: reference %q resolves to %s, which the proxy does not forward", public, r.url, u)
					continue
				}
				internal := strings.TrimPrefix(u.Path, "/ext")
				want := upstreamSRI425["swagger-ui-dist@5.33.1/"+path.Base(internal)]
				if want == "" {
					t.Errorf("reference %q is not a Swagger UI file", r.url)
					continue
				}
				rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", internal)
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				if got := sri425(rec.Body); got != want {
					t.Errorf("GET %s (public %s): body %s, want the upstream file %s", internal, u.Path, got, want)
				}
			}
		})
	}
}

// embeddedBundleURL425 returns the path of the bundle script in the default
// page, resolved against the page URL, failing the test if the page loads
// it from another origin.
func embeddedBundleURL425(t *testing.T) string {
	t.Helper()
	for _, r := range pageRefs425(servePage(t, Config{SpecContent: jsonSpec})) {
		if path.Base(r.url) == "swagger-ui-bundle.js" {
			u := resolve425(t, "http://app.test/swagger/", r.url)
			if u.Host != "app.test" || !strings.HasPrefix(u.Path, "/swagger/") {
				t.Fatalf("the default page loads the bundle from %q (%s), not from /swagger/", r.url, u)
			}
			return u.Path
		}
	}
	t.Fatal("the default page has no swagger-ui-bundle.js reference")
	return ""
}

// TestEmbeddedAssetMethods425: an embedded file answers GET and HEAD and
// refuses other methods like the page and the spec do.
func TestEmbeddedAssetMethods425(t *testing.T) {
	t.Parallel()
	u := embeddedBundleURL425(t)
	mw := New(Config{SpecContent: jsonSpec})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", u)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeaderContains(t, rec, "content-type", "text/javascript")

	_, err = testutil.RunMiddlewareWithMethod(t, mw, "POST", u)
	testutil.AssertHTTPError(t, err, 405)
}

// TestEmbeddedLicenceNoticesServed425: the upstream licence notices are
// served next to the bundles, including the file each bundle's first line
// points at.
func TestEmbeddedLicenceNoticesServed425(t *testing.T) {
	t.Parallel()
	u := embeddedBundleURL425(t)
	dir := path.Dir(u)
	mw := New(Config{SpecContent: jsonSpec})
	get := func(p string) string {
		t.Helper()
		rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", p)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		testutil.AssertHeaderContains(t, rec, "content-type", "text/")
		return rec.BodyString()
	}

	bundle := get(u)
	m := regexp.MustCompile(`^/\*! For license information please see (\S+) \*/`).FindStringSubmatch(bundle)
	if m == nil {
		t.Fatalf("bundle does not start with its licence pointer: %.80q", bundle)
	}
	if got := get(dir + "/" + m[1]); !strings.Contains(got, "MIT") {
		t.Errorf("%s: no MIT notice in %.80q", m[1], got)
	}
	if got := get(dir + "/swagger-ui-standalone-preset.js.LICENSE.txt"); !strings.Contains(got, "@license") {
		t.Errorf("preset notices: %.80q", got)
	}
	if got := get(dir + "/LICENSE"); !strings.Contains(got, "Apache License") || !strings.Contains(got, "Version 2.0") {
		t.Errorf("LICENSE is not the Apache-2.0 text: %.80q", got)
	}
	if got := get(dir + "/NOTICE"); got != "swagger-ui\nCopyright 2020-2021 SmartBear Software Inc.\n" {
		t.Errorf("NOTICE = %q, want the upstream swagger-ui NOTICE", got)
	}
}

// TestUnknownAssetPathPassesThrough425: only the embedded files are
// answered; other paths under the assets prefix reach the next handler, and
// so does every other path when BasePath is "/".
func TestUnknownAssetPathPassesThrough425(t *testing.T) {
	t.Parallel()
	u := embeddedBundleURL425(t)
	mw := New(Config{SpecContent: jsonSpec})
	root := New(Config{SpecContent: jsonSpec, BasePath: "/"})
	for _, c := range []struct {
		mw celeris.HandlerFunc
		p  string
	}{
		{mw, path.Dir(u) + "/swagger-ui-bundle.js.map"},
		{mw, path.Dir(u) + "/"},
		{mw, "/swagger/assets/swagger-ui-dist@0.0.0/swagger-ui-bundle.js"},
		{mw, "/swagger/assets/"},
		{root, "/api/users"},
		{root, "/swagger-ui-bundle.js"},
		{root, "/assets/swagger-ui-dist@0.0.0/swagger-ui-bundle.js"},
		{root, strings.TrimPrefix(u, "/swagger") + ".map"},
	} {
		rec, err := testutil.RunChain(t, []celeris.HandlerFunc{c.mw, okHandler}, "GET", c.p)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		if rec.BodyString() != "ok" {
			t.Errorf("GET %s was answered by the middleware (%d bytes), want the next handler", c.p, len(rec.Body))
		}
	}
}

// TestRendererWithoutEmbeddedAssetsPanics425: Scalar and ReDoc are not
// embedded, so a config that picks neither the CDN nor AssetsPath fails at
// startup instead of silently loading third-party JavaScript.
func TestRendererWithoutEmbeddedAssetsPanics425(t *testing.T) {
	t.Parallel()
	for _, r := range []UIRenderer{RendererScalar, RendererReDoc} {
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			var msg string
			func() {
				defer func() {
					if v := recover(); v != nil {
						msg, _ = v.(string)
						if msg == "" {
							msg = "non-string panic"
						}
					}
				}()
				New(Config{SpecContent: jsonSpec, Renderer: r})
			}()
			if msg == "" {
				t.Fatalf("New with Renderer %q and no asset source did not panic", r)
			}
			for _, want := range []string{string(r), "CDN", "AssetsPath"} {
				if !strings.Contains(msg, want) {
					t.Errorf("panic %q does not mention %q", msg, want)
				}
			}
		})
	}
}
