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

	// OAuth2RedirectURL sets the OAuth2 redirect URL for Swagger UI.
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	OAuth2RedirectURL string

	// OAuth2 pre-fills the OAuth2 authorization dialog in Swagger UI.
	// WARNING: all values including ClientSecret are embedded in the HTML
	// page source and visible to anyone who can access the page. Only use
	// ClientSecret for development or test environments. In production,
	// use PKCE (public clients) which do not require a secret.
	// When nil, no OAuth2 initialization is emitted.
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	OAuth2 *OAuth2Config

	// Title is the HTML page title. Default: "API Documentation".
	Title string
}

// OAuth2Config pre-fills the OAuth2 authorization dialog in Swagger UI.
type OAuth2Config struct {
	// ClientID is the OAuth2 client identifier.
	ClientID string
	// ClientSecret is the OAuth2 client secret. Only for confidential clients.
	ClientSecret string
	// Realm is the OAuth2 realm.
	Realm string
	// AppName is the application name shown in the authorization dialog.
	AppName string
	// Scopes lists the default OAuth2 scopes to request.
	Scopes []string
}

// Config defines the swagger middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match on c.Path()).
	SkipPaths []string

	// BasePath is the URL prefix for the swagger endpoints.
	// Default: "/swagger".
	// The middleware registers:
	//   {BasePath}/         — UI page
	//   {BasePath}/spec     — raw spec file
	BasePath string

	// SpecContent is the raw OpenAPI specification content (JSON or YAML).
	// Either SpecContent or SpecURL must be set.
	SpecContent []byte

	// SpecURL is a URL to an externally hosted spec file. When set,
	// SpecContent is ignored and no /spec endpoint is registered.
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

	// AssetsPath, when set, serves Swagger UI assets from a local path
	// instead of the default CDN. The HTML template references scripts and
	// stylesheets from this prefix (e.g. {AssetsPath}/swagger-ui-bundle.js).
	//
	// Users must serve the assets themselves, for example with a static
	// file middleware:
	//
	//   server.Use(static.New(static.Config{Root: "./swagger-ui-dist", Prefix: "/swagger-assets"}))
	//   server.Use(swagger.New(swagger.Config{
	//       SpecContent: spec,
	//       AssetsPath:  "/swagger-assets",
	//   }))
	AssetsPath string
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
	case RendererSwaggerUI, RendererScalar, RendererReDoc:
		// valid
	default:
		panic(fmt.Sprintf("swagger: unknown Renderer %q", cfg.Renderer))
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
