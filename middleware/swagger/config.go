package swagger

import (
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
	// Default 1 (nil). Set to -1 to completely hide models.
	// Use a pointer to distinguish zero (valid: "expand 0 levels") from
	// unset (nil: use default 1).
	// Swagger UI only; ignored when Renderer is Scalar or ReDoc.
	DefaultModelsExpandDepth *int

	// Title is the HTML page title. Default: "API Documentation".
	Title string
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

var defaultConfig = Config{
	BasePath: "/swagger",
	Renderer: RendererSwaggerUI,
	UI: UIConfig{
		DocExpansion: "list",
		Title:        "API Documentation",
	},
}

func intPtr(v int) *int { return &v }

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
	if cfg.UI.DefaultModelsExpandDepth == nil {
		cfg.UI.DefaultModelsExpandDepth = intPtr(1)
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
