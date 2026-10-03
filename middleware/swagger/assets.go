package swagger

import (
	_ "embed" // go:embed of the Swagger UI files
	"strings"
)

// The Swagger UI files are embedded byte for byte as published in the
// swagger-ui-dist npm package at SwaggerUIVersion (assets/update.go copies
// them and pins.go records their hashes), together with the upstream
// licence notices: swagger-ui is Apache-2.0 (LICENSE, NOTICE) and the
// bundles carry third-party notices (*.LICENSE.txt). The notices are served
// next to the bundles, so a binary that serves the UI also serves them.
//
// The package's OAuth2 redirect page and its script are embedded too
// (celeris#850). The page calls window.opener.swaggerUIRedirectOauth2, so it
// must come from the UI page's origin: it is served at
// {BasePath}/oauth2-redirect.html, where Swagger UI looks for it by default,
// whichever source the UI's own files load from.
//
// Only Swagger UI, the default renderer, is embedded. The Scalar and ReDoc
// bundles (4.4 MB and 1.1 MB) would be linked into every binary that
// imports this package whichever renderer it uses, since the renderer is a
// run-time choice; they load from the pinned CDN (Config.CDN) or from
// Config.AssetsPath instead.
var (
	//go:embed assets/swagger-ui-dist/swagger-ui.css
	swaggerUICSS []byte
	//go:embed assets/swagger-ui-dist/swagger-ui-bundle.js
	swaggerUIBundle []byte
	//go:embed assets/swagger-ui-dist/swagger-ui-standalone-preset.js
	swaggerUIPreset []byte
	//go:embed assets/swagger-ui-dist/oauth2-redirect.html
	swaggerUIOAuth2RedirectPage []byte
	//go:embed assets/swagger-ui-dist/oauth2-redirect.js
	swaggerUIOAuth2RedirectScript []byte
	//go:embed assets/swagger-ui-dist/swagger-ui-bundle.js.LICENSE.txt
	swaggerUIBundleNotices []byte
	//go:embed assets/swagger-ui-dist/swagger-ui-standalone-preset.js.LICENSE.txt
	swaggerUIPresetNotices []byte
	//go:embed assets/swagger-ui-dist/LICENSE
	swaggerUILicense []byte
	//go:embed assets/swagger-ui-dist/NOTICE
	swaggerUINotice []byte
)

const (
	contentTypeCSS  = "text/css; charset=utf-8"
	contentTypeHTML = "text/html; charset=utf-8"
	contentTypeJS   = "text/javascript; charset=utf-8"
	contentTypeText = "text/plain; charset=utf-8"

	// assetCacheControl lets browsers keep an embedded file for a year
	// without revalidating: its URL carries the version, and a version's
	// bytes never change.
	assetCacheControl = "public, max-age=31536000, immutable"

	cdnBase = "https://cdn.jsdelivr.net/npm/"
)

// embeddedAsset is one file served under the embedded assets prefix.
type embeddedAsset struct {
	name        string
	contentType string
	body        []byte
}

var swaggerUIAssets = []embeddedAsset{
	{"swagger-ui.css", contentTypeCSS, swaggerUICSS},
	{"swagger-ui-bundle.js", contentTypeJS, swaggerUIBundle},
	{"swagger-ui-standalone-preset.js", contentTypeJS, swaggerUIPreset},
	{"swagger-ui-bundle.js.LICENSE.txt", contentTypeText, swaggerUIBundleNotices},
	{"swagger-ui-standalone-preset.js.LICENSE.txt", contentTypeText, swaggerUIPresetNotices},
	{"LICENSE", contentTypeText, swaggerUILicense},
	{"NOTICE", contentTypeText, swaggerUINotice},
}

// The OAuth2 redirect page and its script, served next to the UI page at
// {BasePath}/oauth2-redirect.html and {BasePath}/oauth2-redirect.js: Swagger
// UI's default oauth2RedirectUrl is the page's directory plus
// "oauth2-redirect.html", and the redirect page loads "oauth2-redirect.js"
// relative to itself. Their URLs carry no version, so they get no cache
// lifetime, like the page.
var (
	oauth2RedirectPage   = embeddedAsset{"oauth2-redirect.html", contentTypeHTML, swaggerUIOAuth2RedirectPage}
	oauth2RedirectScript = embeddedAsset{"oauth2-redirect.js", contentTypeJS, swaggerUIOAuth2RedirectScript}
)

// embeddedAssetsDir is where the embedded Swagger UI files live, relative
// to the page at {BasePath}/. The version in the path makes each URL name
// one immutable file, so an upgrade is never served from a stale cache.
const embeddedAssetsDir = "assets/swagger-ui-dist@" + SwaggerUIVersion

// embeddedAssetsPrefix is the URL path prefix of the embedded Swagger UI
// files for a trimmed base path.
func embeddedAssetsPrefix(basePath string) string {
	return basePath + "/" + embeddedAssetsDir
}

// embeddedAssetRoutes maps each embedded file's full URL path to it.
func embeddedAssetRoutes(basePath string) map[string]embeddedAsset {
	prefix := embeddedAssetsPrefix(basePath) + "/"
	routes := make(map[string]embeddedAsset, len(swaggerUIAssets))
	for _, a := range swaggerUIAssets {
		routes[prefix+a.name] = a
	}
	return routes
}

// assetRef is a stylesheet or script reference in a page. Integrity is set
// only for the CDN, whose bytes this package does not serve itself; the
// tag then also carries crossorigin="anonymous", which a cross-origin
// integrity check requires.
type assetRef struct {
	URL       string
	Integrity string
}

// swaggerUIRefs returns the stylesheet, bundle and preset references for
// the configured asset source.
func swaggerUIRefs(cfg Config) (css, bundle, preset assetRef) {
	switch {
	case cfg.AssetsPath != "":
		p := strings.TrimRight(cfg.AssetsPath, "/")
		return assetRef{URL: p + "/swagger-ui.css"},
			assetRef{URL: p + "/swagger-ui-bundle.js"},
			assetRef{URL: p + "/swagger-ui-standalone-preset.js"}
	case cfg.CDN:
		p := cdnBase + "swagger-ui-dist@" + SwaggerUIVersion
		return assetRef{p + "/swagger-ui.css", sriSwaggerUICSS},
			assetRef{p + "/swagger-ui-bundle.js", sriSwaggerUIBundle},
			assetRef{p + "/swagger-ui-standalone-preset.js", sriSwaggerUIPreset}
	default:
		// Relative to the page, which is served only at {BasePath}/, so
		// the files also load when a reverse proxy serves the page under
		// another prefix (/ext/swagger/ forwarded to /swagger/).
		p := embeddedAssetsDir
		return assetRef{URL: p + "/swagger-ui.css"},
			assetRef{URL: p + "/swagger-ui-bundle.js"},
			assetRef{URL: p + "/swagger-ui-standalone-preset.js"}
	}
}

// scalarRef returns the Scalar script reference. validate() has ensured
// AssetsPath or CDN is set: Scalar is not embedded. With AssetsPath the file
// keeps the name @scalar/api-reference publishes it under,
// dist/browser/standalone.js (celeris#882).
func scalarRef(cfg Config) assetRef {
	if cfg.AssetsPath != "" {
		return assetRef{URL: strings.TrimRight(cfg.AssetsPath, "/") + "/standalone.js"}
	}
	return assetRef{cdnBase + "@scalar/api-reference@" + ScalarVersion + "/dist/browser/standalone.js", sriScalar}
}

// redocRef returns the ReDoc script reference. validate() has ensured
// AssetsPath or CDN is set: ReDoc is not embedded.
func redocRef(cfg Config) assetRef {
	if cfg.AssetsPath != "" {
		return assetRef{URL: strings.TrimRight(cfg.AssetsPath, "/") + "/redoc.standalone.js"}
	}
	return assetRef{cdnBase + "redoc@" + ReDocVersion + "/bundles/redoc.standalone.js", sriReDoc}
}
