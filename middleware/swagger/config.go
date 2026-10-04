package swagger

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/goceleris/celeris"
)

// UIRenderer selects the frontend used to render the API specification.
type UIRenderer string

const (
	// RendererSwaggerUI uses Swagger UI (default).
	RendererSwaggerUI UIRenderer = "swagger-ui"
	// RendererScalar uses Scalar API reference.
	RendererScalar UIRenderer = "scalar"
	// RendererReDoc uses ReDoc API reference.
	RendererReDoc UIRenderer = "redoc"
)

// UIConfig controls the appearance and behavior of the Swagger UI or
// Scalar renderer.
type UIConfig struct {
	// DocExpansion controls how operations are displayed on first load.
	// Valid values: "list" (default, expand tags), "full" (expand everything),
	// "none" (collapse all).
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	DocExpansion string

	// DeepLinking enables deep linking for tags and operations.
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	DeepLinking bool

	// PersistAuthorization persists authorization data across browser sessions.
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	PersistAuthorization bool

	// DefaultModelsExpandDepth controls how deep models are expanded.
	// Default: nil (Swagger UI default, which is 1). Use [IntPtr] to set
	// an explicit value: IntPtr(0) for model names only, IntPtr(-1) to
	// hide the models section entirely.
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	DefaultModelsExpandDepth *int

	// OAuth2RedirectURL sets the OAuth2 redirect URL for Swagger UI: the
	// redirect_uri its authorization-code and implicit flows send. When
	// empty (the default), Swagger UI uses the page's directory plus
	// oauth2-redirect.html, which is {BasePath}/oauth2-redirect.html on the
	// page's own origin, and this middleware serves that page (the one
	// shipped in swagger-ui-dist, with its script at
	// {BasePath}/oauth2-redirect.js) whatever the UI's files load from.
	// Register that URL with the authorization server. Set this only to use
	// another redirect page; it must be an absolute URL on the page's
	// origin, as the redirect page hands the result to the UI page through
	// window.opener. An app that serves its own page at
	// {BasePath}/oauth2-redirect.html lists both paths in SkipPaths:
	// otherwise this middleware answers them, and the app's route for them
	// does not run (a handler that answers ends the chain, celeris#927).
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	OAuth2RedirectURL string

	// ValidatorURL is the online validator Swagger UI's validator badge
	// sends the spec's absolute URL to. When empty (the default), the page
	// sets validatorUrl "none", which turns the badge off, so a viewer's
	// browser contacts no validator. Set it to a validator you trust to
	// show the badge; Swagger UI's own default is
	// https://validator.swagger.io/validator, a third party that then also
	// fetches the spec itself when it can reach it.
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	ValidatorURL string

	// OAuth2 pre-fills the OAuth2 authorization dialog in Swagger UI.
	// All values are embedded in the served HTML page source and visible
	// to anyone who can access the page; only public-client material is
	// supported (no ClientSecret — use PKCE via [OAuth2Config.UsePKCE]).
	// When nil, no OAuth2 initialization is emitted. Swagger UI only;
	// ignored when Renderer is Scalar or ReDoc.
	OAuth2 *OAuth2Config

	// Title is the HTML page title. Default: "API Documentation".
	Title string
}

// OAuth2Config pre-fills the OAuth2 authorization dialog in Swagger UI.
//
// Browser flows MUST use PKCE — the public ClientID is the only secret
// material safe to embed in HTML. Confidential-client secrets cannot be
// kept secret in a browser; the previous ClientSecret field was removed
// in v1.3.4 because it shipped credentials in plaintext to every page
// load. If you have a server-side OAuth flow, perform the token exchange
// on your backend, not in Swagger UI.
type OAuth2Config struct {
	// ClientID is the OAuth2 client identifier (public; safe to embed).
	ClientID string
	// Realm is the OAuth2 realm.
	Realm string
	// AppName is the application name shown in the authorization dialog.
	AppName string
	// Scopes lists the default OAuth2 scopes to request.
	Scopes []string
	// UsePKCE enables Proof Key for Code Exchange (RFC 7636), the
	// recommended public-client flow. Default: true.
	UsePKCE bool
}

// Config defines the swagger middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match on c.Path()).
	SkipPaths []string

	// BasePath is the URL prefix for the swagger endpoints.
	// Default: "/swagger".
	// The middleware answers:
	//   {BasePath}           — redirect to {BasePath}/
	//   {BasePath}/          — UI page
	//   {BasePath}/spec      — raw spec file
	//   {BasePath}/assets/swagger-ui-dist@{SwaggerUIVersion}/*
	//                        — the embedded Swagger UI files, when the
	//                          page uses them (see CDN)
	//   {BasePath}/oauth2-redirect.html, {BasePath}/oauth2-redirect.js
	//                        — Swagger UI's OAuth2 redirect page (see
	//                          UIConfig.OAuth2RedirectURL)
	//
	// Every one of these requests must reach the middleware: a router
	// rule, route list or proxy that forwards only {BasePath}/ and
	// {BasePath}/spec leaves the default page blank.
	//
	// The page refers to the spec, the embedded files and the redirect
	// page relative to itself, and the redirect's Location is relative
	// too ("./swagger/" for /swagger), so the defaults also work behind a
	// reverse proxy that publishes the app under another path prefix and
	// strips it.
	BasePath string

	// SpecContent is the raw OpenAPI specification content (JSON or YAML).
	// Either SpecContent or SpecURL must be set.
	SpecContent []byte

	// SpecURL is a URL to an externally hosted spec file. When set,
	// SpecContent is ignored and no /spec endpoint is registered. When
	// empty, the page loads the spec from "spec", relative to the page
	// at {BasePath}/, which is {BasePath}/spec.
	//
	// Scalar reads it from a URL attribute (data-url), where only relative,
	// http and https URLs are supported; other schemes are neutralised
	// by html/template. Swagger UI and ReDoc receive it as a JS string.
	SpecURL string

	// SpecFile is the original filename of the spec (e.g. "openapi.yaml").
	// Used as a hint for content-type detection when SpecContent is provided.
	// When omitted, content-type is detected from the spec bytes.
	SpecFile string

	// Renderer selects the UI renderer. Default: RendererSwaggerUI.
	Renderer UIRenderer

	// UI controls the appearance and behavior of the UI renderer.
	UI UIConfig

	// Options provides renderer-specific configuration as a JSON-serializable
	// map. For Swagger UI, these are passed to SwaggerUIBundle(). For ReDoc,
	// these are passed to Redoc.init(). For Scalar, these are passed as
	// data-configuration. When nil, renderer defaults are used.
	//
	// Example (ReDoc dark theme):
	//
	//	swagger.Config{
	//	    SpecContent: spec,
	//	    Renderer:    swagger.RendererReDoc,
	//	    Options: map[string]any{
	//	        "theme": map[string]any{
	//	            "colors": map[string]any{"primary": map[string]any{"main": "#32329f"}},
	//	        },
	//	        "expandResponses": "200,201",
	//	        "hideDownloadButton": true,
	//	    },
	//	}
	Options map[string]any

	// AssetsPath, when set, makes the page load the renderer's files from
	// this URL prefix, which you serve yourself, instead of the embedded
	// copy or the CDN. The page references:
	//
	//   Swagger UI: {AssetsPath}/swagger-ui.css, {AssetsPath}/swagger-ui-bundle.js
	//               and {AssetsPath}/swagger-ui-standalone-preset.js
	//               (swagger-ui-dist's files of those names)
	//   Scalar:     {AssetsPath}/standalone.js
	//               (@scalar/api-reference's dist/browser/standalone.js)
	//   ReDoc:      {AssetsPath}/redoc.standalone.js
	//               (redoc's bundles/redoc.standalone.js)
	//
	// The page is written for the versions in [SwaggerUIVersion],
	// [ScalarVersion] and [ReDocVersion]. No integrity hash is emitted, as
	// the files are yours. Swagger UI's OAuth2 redirect page is still
	// served by this middleware (see UIConfig.OAuth2RedirectURL). For
	// example, with the static middleware:
	//
	//   server.Use(static.New(static.Config{Root: "./swagger-ui-dist", Prefix: "/swagger-assets"}))
	//   server.Use(swagger.New(swagger.Config{
	//       SpecContent: spec,
	//       AssetsPath:  "/swagger-assets",
	//   }))
	//
	// AssetsPath and CDN are mutually exclusive.
	AssetsPath string

	// CDN, when true, makes the page load the renderer's files from the
	// jsDelivr CDN (cdn.jsdelivr.net), pinned to the exact versions in
	// [SwaggerUIVersion], [ScalarVersion] and [ReDocVersion], with a
	// Subresource Integrity hash on every script and stylesheet: the
	// browser refuses a file whose bytes differ from the release this
	// package was built against. The page then needs cdn.jsdelivr.net in
	// its Content-Security-Policy and the viewer's browser needs Internet
	// access. Wherever it is loaded from, Scalar's bundle also names
	// fonts.scalar.com (its default fonts, Options "withDefaultFonts")
	// and proxy.scalar.com (its request proxy, Options "proxyUrl").
	// Default: false.
	//
	// By default Swagger UI is served from a copy embedded in this package,
	// under {BasePath}/assets/swagger-ui-dist@{SwaggerUIVersion}/ (with the
	// upstream LICENSE and NOTICE files), so the page loads no script or
	// stylesheet from a third-party origin. The page references the files
	// relative to {BasePath}/, so they also load behind a reverse proxy
	// that serves the page under another prefix. Scalar and ReDoc are not
	// embedded: they need CDN or AssetsPath, and New panics if neither is
	// set.
	CDN bool
}

// IntPtr returns a pointer to v. Use with [UIConfig].DefaultModelsExpandDepth
// to distinguish explicit zero from unset:
//
//	swagger.IntPtr(0)  // show model names only
//	swagger.IntPtr(-1) // hide models section
func IntPtr(v int) *int { return &v }

var defaultConfig = Config{
	BasePath: "/swagger",
	Renderer: RendererSwaggerUI,
	UI: UIConfig{
		DocExpansion: "list",
		Title:        "API Documentation",
	},
}

func applyDefaults(cfg Config) Config {
	if cfg.BasePath == "" {
		cfg.BasePath = defaultConfig.BasePath
	}
	if cfg.Renderer == "" {
		cfg.Renderer = defaultConfig.Renderer
	}
	if cfg.UI.DocExpansion == "" {
		cfg.UI.DocExpansion = defaultConfig.UI.DocExpansion
	}
	if cfg.UI.Title == "" {
		cfg.UI.Title = defaultConfig.UI.Title
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.BasePath == "" || cfg.BasePath[0] != '/' {
		panic("swagger: BasePath must start with '/'")
	}
	if cfg.SpecContent == nil && cfg.SpecURL == "" {
		panic("swagger: either SpecContent or SpecURL must be set")
	}
	switch cfg.Renderer {
	case RendererSwaggerUI:
		// valid; embedded unless AssetsPath or CDN is set
	case RendererScalar, RendererReDoc:
		if cfg.AssetsPath == "" && !cfg.CDN {
			panic(fmt.Sprintf("swagger: Renderer %q has no embedded assets; set CDN: true to load it from jsDelivr "+
				"(pinned, with an integrity hash) or AssetsPath to serve the files yourself", cfg.Renderer))
		}
	default:
		panic(fmt.Sprintf("swagger: unknown Renderer %q", cfg.Renderer))
	}
	if cfg.AssetsPath != "" && cfg.CDN {
		panic("swagger: AssetsPath and CDN are mutually exclusive")
	}
	switch cfg.UI.DocExpansion {
	case "list", "full", "none":
		// valid
	default:
		panic(fmt.Sprintf("swagger: unknown DocExpansion %q", cfg.UI.DocExpansion))
	}
	if cfg.Options != nil {
		if _, err := json.Marshal(cfg.Options); err != nil {
			panic(fmt.Sprintf("swagger: Options is not JSON-serializable: %v", err))
		}
	}
}

// detectSpecContentType determines the MIME type for the spec content.
// It checks the file extension first, then falls back to inspecting the
// first non-whitespace byte of the content.
func detectSpecContentType(content []byte, filename string) string {
	if filename != "" {
		ext := strings.ToLower(filepath.Ext(filename))
		switch ext {
		case ".json":
			return "application/json"
		case ".yaml", ".yml":
			return "application/x-yaml"
		}
	}
	for _, b := range content {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return "application/json"
		default:
			return "application/x-yaml"
		}
	}
	return "application/json"
}
