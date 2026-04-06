package swagger

import (
	"fmt"
	"strings"

	"github.com/goceleris/celeris"
)

// New creates a swagger middleware that serves an OpenAPI spec viewer.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	basePath := strings.TrimRight(cfg.BasePath, "/")
	uiPath := basePath + "/"
	specPath := basePath + "/spec"

	var specContentType string
	if cfg.SpecContent != nil {
		specContentType = detectSpecContentType(cfg.SpecContent, cfg.SpecFile)
	}

	specURL := cfg.SpecURL
	if specURL == "" {
		specURL = specPath
	}

	page := buildPage(cfg, specURL)

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()

		if path != basePath && path != uiPath && path != specPath {
			return c.Next()
		}

		method := c.Method()
		if method != "GET" && method != "HEAD" {
			return c.NoContent(405)
		}

		switch path {
		case basePath:
			c.SetHeader("location", uiPath)
			return c.NoContent(301)
		case uiPath:
			return c.HTML(200, page)
		case specPath:
			if cfg.SpecContent == nil {
				return c.NoContent(404)
			}
			return c.Blob(200, specContentType, cfg.SpecContent)
		}

		return c.Next()
	}
}

// buildPage generates the HTML page for the configured renderer.
func buildPage(cfg Config, specURL string) string {
	switch cfg.Renderer {
	case RendererScalar:
		return buildScalarPage(cfg, specURL)
	default:
		return buildSwaggerUIPage(cfg, specURL)
	}
}

func buildSwaggerUIPage(cfg Config, specURL string) string {
	ui := cfg.UI

	var cssURL, bundleURL, presetURL string
	if cfg.AssetsPath != "" {
		base := strings.TrimRight(cfg.AssetsPath, "/")
		cssURL = base + "/swagger-ui.css"
		bundleURL = base + "/swagger-ui-bundle.js"
		presetURL = base + "/swagger-ui-standalone-preset.js"
	} else {
		const cdn = "https://unpkg.com/swagger-ui-dist@5"
		cssURL = cdn + "/swagger-ui.css"
		bundleURL = cdn + "/swagger-ui-bundle.js"
		presetURL = cdn + "/swagger-ui-standalone-preset.js"
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<link rel="stylesheet" href="%s">
</head>
<body>
<div id="swagger-ui"></div>
<script src="%s"></script>
<script src="%s"></script>
<script>
SwaggerUIBundle({
  url: %q,
  dom_id: "#swagger-ui",
  presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
  layout: "StandaloneLayout",
  docExpansion: %q,
  deepLinking: %v,
  persistAuthorization: %v,
  defaultModelsExpandDepth: %d
});
</script>
</body>
</html>`, ui.Title, cssURL, bundleURL, presetURL,
		specURL, ui.DocExpansion, ui.DeepLinking, ui.PersistAuthorization, ui.DefaultModelsExpandDepth)
}

func buildScalarPage(cfg Config, specURL string) string {
	ui := cfg.UI

	var scriptTag string
	if cfg.AssetsPath != "" {
		base := strings.TrimRight(cfg.AssetsPath, "/")
		scriptTag = fmt.Sprintf(`<script id="api-reference" data-url=%q data-configuration='{"theme":"default"}'></script>
<script src="%s/standalone.min.js"></script>`, specURL, base)
	} else {
		scriptTag = fmt.Sprintf(`<script id="api-reference" data-url=%q data-configuration='{"theme":"default"}'></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>`, specURL)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
</head>
<body>
%s
</body>
</html>`, ui.Title, scriptTag)
}
