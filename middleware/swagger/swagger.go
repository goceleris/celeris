package swagger

import (
	"fmt"
	"html"
	"io"
	"strings"

	"github.com/goceleris/celeris"
)

const swaggerUITemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Swagger UI</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@latest/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@latest/swagger-ui-bundle.js"></script>
<script>
SwaggerUIBundle({
  url: "%s",
  dom_id: "#swagger-ui",
  presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
  layout: "BaseLayout"
});
</script>
</body>
</html>`

const scalarTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>API Reference</title>
</head>
<body>
<script id="api-reference" data-url="%s"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@latest"></script>
</body>
</html>`

// New creates a swagger documentation middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	specBytes := loadSpec(cfg)

	// Escape SpecURL for safe embedding in HTML/JavaScript contexts.
	// Prevents XSS if SpecURL contains quotes or script tags.
	safeURL := html.EscapeString(cfg.SpecURL)

	var uiHTML string
	switch cfg.UIEngine {
	case "scalar":
		uiHTML = fmt.Sprintf(scalarTemplate, safeURL)
	default:
		uiHTML = fmt.Sprintf(swaggerUITemplate, safeURL)
	}

	uiPath := strings.TrimRight(cfg.UIPath, "/")
	uiPathSlash := uiPath + "/"
	specURL := cfg.SpecURL
	authFunc := cfg.AuthFunc

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		path := c.Path()

		if skip.ShouldSkip(c) {
			return c.Next()
		}

		if path != uiPath && path != uiPathSlash && path != specURL {
			return c.Next()
		}

		if authFunc != nil && !authFunc(c) {
			return c.NoContent(403)
		}

		if path == specURL {
			return c.Blob(200, "application/json", specBytes)
		}

		return c.HTML(200, uiHTML)
	}
}

func loadSpec(cfg Config) []byte {
	if cfg.SpecContent != nil {
		cp := make([]byte, len(cfg.SpecContent))
		copy(cp, cfg.SpecContent)
		return cp
	}
	f, err := cfg.Filesystem.Open(cfg.SpecFile)
	if err != nil {
		panic("swagger: failed to open spec from Filesystem: " + err.Error())
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		panic("swagger: failed to read spec from Filesystem: " + err.Error())
	}
	return data
}
