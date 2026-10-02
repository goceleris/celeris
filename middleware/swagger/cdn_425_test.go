package swagger

import (
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// celeris#425: the CDN is opt-in, pinned to exact versions and guarded by
// Subresource Integrity.

var cdnURLRe425 = regexp.MustCompile(`^https://cdn\.jsdelivr\.net/npm/((?:@[a-z0-9-]+/)?[a-z0-9-]+@[0-9]+\.[0-9]+\.[0-9]+)/(.+)$`)

// TestCDNPinnedWithIntegrity425: with CDN set, every reference of every
// renderer names an exact x.y.z version on jsDelivr and carries the
// upstream file's sha384 with crossorigin="anonymous".
func TestCDNPinnedWithIntegrity425(t *testing.T) {
	t.Parallel()
	cases := []struct {
		renderer UIRenderer
		want     []string // package@version/path
	}{
		{RendererSwaggerUI, []string{
			"swagger-ui-dist@" + SwaggerUIVersion + "/swagger-ui.css",
			"swagger-ui-dist@" + SwaggerUIVersion + "/swagger-ui-bundle.js",
			"swagger-ui-dist@" + SwaggerUIVersion + "/swagger-ui-standalone-preset.js",
		}},
		{RendererScalar, []string{"@scalar/api-reference@" + ScalarVersion + "/dist/browser/standalone.js"}},
		{RendererReDoc, []string{"redoc@" + ReDocVersion + "/bundles/redoc.standalone.js"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.renderer), func(t *testing.T) {
			t.Parallel()
			refs := pageRefs425(servePage(t, Config{SpecContent: jsonSpec, Renderer: tc.renderer, CDN: true}))
			if len(refs) != len(tc.want) {
				t.Fatalf("page has %d references, want %d: %+v", len(refs), len(tc.want), refs)
			}
			for i, r := range refs {
				m := cdnURLRe425.FindStringSubmatch(r.url)
				if m == nil {
					t.Errorf("reference %q is not an exact-version jsDelivr URL", r.url)
					continue
				}
				key := m[1] + "/" + m[2]
				if key != tc.want[i] {
					t.Errorf("reference %d = %s, want %s", i, key, tc.want[i])
				}
				want, ok := upstreamSRI425[key]
				if !ok {
					t.Errorf("no upstream hash recorded for %s", key)
				}
				if r.integrity != want {
					t.Errorf("%s: integrity %q, want %q", key, r.integrity, want)
				}
				if r.crossorigin != "anonymous" {
					t.Errorf("%s: crossorigin %q, want \"anonymous\" (needed for a cross-origin integrity check)", key, r.crossorigin)
				}
			}
		})
	}
}

// TestPinsMatchEmbeddedFiles425: the embedded files are the upstream bytes,
// and the pinned CDN hashes in pins.go are the upstream hashes, so the
// embedded copy and the CDN copy are the same release.
func TestPinsMatchEmbeddedFiles425(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for k, v := range upstreamSRI425 {
		if len(v) != len("sha384-")+64 || !strings.HasPrefix(v, "sha384-") {
			t.Errorf("%s: %q is not a sha384 SRI value", k, v)
		}
		if other, dup := seen[v]; dup {
			t.Errorf("%s and %s have the same hash %s", k, other, v)
		}
		seen[v] = k
	}
	sw := "swagger-ui-dist@" + SwaggerUIVersion + "/"
	for _, c := range []struct {
		key, pin string
		body     []byte
	}{
		{sw + "swagger-ui.css", sriSwaggerUICSS, swaggerUICSS},
		{sw + "swagger-ui-bundle.js", sriSwaggerUIBundle, swaggerUIBundle},
		{sw + "swagger-ui-standalone-preset.js", sriSwaggerUIPreset, swaggerUIPreset},
		{"@scalar/api-reference@" + ScalarVersion + "/dist/browser/standalone.js", sriScalar, nil},
		{"redoc@" + ReDocVersion + "/bundles/redoc.standalone.js", sriReDoc, nil},
	} {
		want, ok := upstreamSRI425[c.key]
		if !ok {
			t.Errorf("no upstream hash recorded for %s (the pinned version moved: update upstreamSRI425)", c.key)
			continue
		}
		if c.pin != want {
			t.Errorf("%s: pinned %s, upstream %s", c.key, c.pin, want)
		}
		if c.body != nil {
			if got := sri425(c.body); got != want {
				t.Errorf("%s: embedded bytes hash to %s (%d bytes), upstream %s", c.key, got, len(c.body), want)
			}
		}
	}
}

// TestSelfHostedAndCDNServeNoEmbeddedFiles425: when the page does not load
// the embedded files, their paths reach the next handler.
func TestSelfHostedAndCDNServeNoEmbeddedFiles425(t *testing.T) {
	t.Parallel()
	u := embeddedBundleURL425(t)
	for name, cfg := range map[string]Config{
		"cdn":        {SpecContent: jsonSpec, CDN: true},
		"assetspath": {SpecContent: jsonSpec, AssetsPath: "/static/swagger"},
		"scalar-cdn": {SpecContent: jsonSpec, Renderer: RendererScalar, CDN: true},
	} {
		rec, err := testutil.RunChain(t, []celeris.HandlerFunc{New(cfg), okHandler}, "GET", u)
		testutil.AssertNoError(t, err)
		if rec.BodyString() != "ok" {
			t.Errorf("%s: GET %s was answered by the middleware, want the next handler", name, u)
		}
	}
}

// TestSelfHostedHasNoIntegrity425: AssetsPath files are the user's, of an
// unknown version, so the page must not claim a hash for them.
func TestSelfHostedHasNoIntegrity425(t *testing.T) {
	t.Parallel()
	for _, r := range []UIRenderer{RendererSwaggerUI, RendererScalar, RendererReDoc} {
		refs := pageRefs425(servePage(t, Config{SpecContent: jsonSpec, Renderer: r, AssetsPath: "/static"}))
		if len(refs) == 0 {
			t.Fatalf("%s: no references", r)
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref.url, "/static/") || ref.hasIntegrity || ref.crossorigin != "" {
				t.Errorf("%s: reference %+v, want a plain /static/ path", r, ref)
			}
		}
	}
}

// TestAssetsPathAndCDNExclusive425: both set is a configuration error.
func TestAssetsPathAndCDNExclusive425(t *testing.T) {
	t.Parallel()
	defer func() {
		if v := recover(); v == nil || !strings.Contains(v.(string), "mutually exclusive") {
			t.Fatalf("recover() = %v, want the mutually-exclusive panic", v)
		}
	}()
	New(Config{SpecContent: jsonSpec, AssetsPath: "/static", CDN: true})
}
