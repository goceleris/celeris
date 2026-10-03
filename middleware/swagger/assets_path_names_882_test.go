package swagger

import (
	"path"
	"strings"
	"testing"
)

// celeris#882: with AssetsPath the page must ask for files under the names
// the pinned packages publish them, so a copy of a package's files works.

// pinnedBrowserFiles882 lists, for each renderer, the files of the pinned npm
// package in the directory its browser build is published from
// (swagger-ui-dist 5.33.1 package root, @scalar/api-reference 1.72.4
// dist/browser/, redoc 2.5.4 bundles/), as listed from the tarballs, each
// checked against the registry's sha512 integrity: every .js, .css and
// .html file there (source maps, images and text files are left out).
var pinnedBrowserFiles882 = map[UIRenderer][]string{
	RendererSwaggerUI: {
		"absolute-path.js", "index.css", "index.html", "index.js", "oauth2-redirect.html", "oauth2-redirect.js",
		"swagger-initializer.js", "swagger-ui-bundle.js", "swagger-ui-es-bundle-core.js", "swagger-ui-es-bundle.js",
		"swagger-ui-standalone-preset.js", "swagger-ui.css", "swagger-ui.js",
	},
	RendererScalar: {"standalone.esm.js", "standalone.js"},
	RendererReDoc:  {"redoc.browser.lib.js", "redoc.lib.js", "redoc.standalone.js"},
}

// TestAssetsPathNamesPinnedFiles882: every file an AssetsPath page references
// is one the pinned package publishes, and it is the same file the CDN page
// loads (whose package path and hash TestCDNPinnedWithIntegrity425 checks
// against the tarball).
func TestAssetsPathNamesPinnedFiles882(t *testing.T) {
	t.Parallel()
	for _, r := range []UIRenderer{RendererSwaggerUI, RendererScalar, RendererReDoc} {
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			published := map[string]bool{}
			for _, f := range pinnedBrowserFiles882[r] {
				published[f] = true
			}
			self := pageRefs425(servePage(t, Config{SpecContent: jsonSpec, Renderer: r, AssetsPath: "/static/docs"}))
			cdn := pageRefs425(servePage(t, Config{SpecContent: jsonSpec, Renderer: r, CDN: true}))
			if len(self) == 0 || len(self) != len(cdn) {
				t.Fatalf("AssetsPath page has %d references, CDN page %d", len(self), len(cdn))
			}
			for i, ref := range self {
				name, ok := strings.CutPrefix(ref.url, "/static/docs/")
				if !ok {
					t.Errorf("reference %q is not under AssetsPath", ref.url)
					continue
				}
				if !published[name] {
					t.Errorf("reference %q names %q, which the pinned %s package does not publish (its files: %v)", ref.url, name, r, pinnedBrowserFiles882[r])
				}
				if want := path.Base(cdn[i].url); name != want {
					t.Errorf("reference %q names %q; the CDN page loads %q", ref.url, name, want)
				}
			}
		})
	}
}
