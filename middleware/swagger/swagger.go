package swagger

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/goceleris/celeris"
)

// marshalOptions serializes the Options map to a JSON string for embedding
// in HTML templates. Returns "{}" when opts is nil or empty.
func marshalOptions(opts map[string]any) string {
	if len(opts) == 0 {
		return "{}"
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// jsString renders s as a JavaScript string literal safe to embed inside an
// inline <script> block. json.Marshal is the context-correct escaper here:
// a JSON string is a valid JS string literal, and Go's encoder escapes '<',
// '>' and '&' (so "</script>" cannot terminate the block) as well as U+2028
// and U+2029 (JS line terminators). Go's %q is NOT suitable: it leaves
// "</script>" intact and emits escapes JS lacks (\a, \x, \U).
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail (invalid UTF-8 is replaced
		// with U+FFFD); keep a safe fallback anyway.
		return `""`
	}
	return string(b)
}

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
			return celeris.NewHTTPError(405, "Method Not Allowed")
		}

		switch path {
		case basePath:
			return c.Redirect(301, uiPath)
		case uiPath:
			return c.HTML(200, page)
		case specPath:
			if cfg.SpecContent == nil {
				return celeris.NewHTTPError(404, "Not Found")
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
	case RendererReDoc:
		return buildReDocPage(cfg, specURL)
	default:
		return buildSwaggerUIPage(cfg, specURL)
	}
}

func buildSwaggerUIPage(cfg Config, specURL string) string {
	ui := cfg.UI

	depth := 1 // Swagger UI default
	if ui.DefaultModelsExpandDepth != nil {
		depth = *ui.DefaultModelsExpandDepth
	}

	var cssURL, bundleURL, presetURL string
	if cfg.AssetsPath != "" {
		base := strings.TrimRight(cfg.AssetsPath, "/")
		cssURL = html.EscapeString(base + "/swagger-ui.css")
		bundleURL = html.EscapeString(base + "/swagger-ui-bundle.js")
		presetURL = html.EscapeString(base + "/swagger-ui-standalone-preset.js")
	} else {
		const cdn = "https://unpkg.com/swagger-ui-dist@5"
		cssURL = cdn + "/swagger-ui.css"
		bundleURL = cdn + "/swagger-ui-bundle.js"
		presetURL = cdn + "/swagger-ui-standalone-preset.js"
	}

	var oauth2Redirect string
	if ui.OAuth2RedirectURL != "" {
		oauth2Redirect = fmt.Sprintf(",\n  oauth2RedirectUrl: %s", jsString(ui.OAuth2RedirectURL))
	}

	var oauth2Init string
	if ui.OAuth2 != nil {
		oa := ui.OAuth2
		var oaParts []string
		if oa.ClientID != "" {
			oaParts = append(oaParts, "clientId: "+jsString(oa.ClientID))
		}
		if oa.UsePKCE {
			oaParts = append(oaParts, "usePkceWithAuthorizationCodeGrant: true")
		}
		if oa.Realm != "" {
			oaParts = append(oaParts, "realm: "+jsString(oa.Realm))
		}
		if oa.AppName != "" {
			oaParts = append(oaParts, "appName: "+jsString(oa.AppName))
		}
		if len(oa.Scopes) > 0 {
			oaParts = append(oaParts, "scopes: "+jsString(strings.Join(oa.Scopes, " ")))
		}
		oauth2Init = fmt.Sprintf("\nui.initOAuth({%s});", strings.Join(oaParts, ", "))
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
const ui = SwaggerUIBundle({
  url: %s,
  dom_id: "#swagger-ui",
  presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
  layout: "StandaloneLayout",
  docExpansion: %s,
  deepLinking: %v,
  persistAuthorization: %v,
  defaultModelsExpandDepth: %d%s
});%s
</script>
</body>
</html>`, html.EscapeString(ui.Title), cssURL, bundleURL, presetURL,
		jsString(specURL), jsString(ui.DocExpansion), ui.DeepLinking, ui.PersistAuthorization,
		depth, oauth2Redirect, oauth2Init)
}

func buildScalarPage(cfg Config, specURL string) string {
	ui := cfg.UI

	scalarOpts := cfg.Options
	if scalarOpts == nil {
		scalarOpts = map[string]any{"theme": "default"}
	}
	// Both data-* values are HTML attribute values: html.EscapeString is the
	// context-correct escaper (Go's %q would leave '&' raw and emit \" which
	// HTML does not honour).
	dataURL := html.EscapeString(specURL)
	dataCfg := html.EscapeString(marshalOptions(scalarOpts))

	var scriptTag string
	if cfg.AssetsPath != "" {
		base := strings.TrimRight(cfg.AssetsPath, "/")
		scriptTag = fmt.Sprintf(`<script id="api-reference" data-url="%s" data-configuration='%s'></script>
<script src="%s/standalone.min.js"></script>`, dataURL, dataCfg, html.EscapeString(base))
	} else {
		scriptTag = fmt.Sprintf(`<script id="api-reference" data-url="%s" data-configuration='%s'></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1"></script>`, dataURL, dataCfg)
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
</html>`, html.EscapeString(ui.Title), scriptTag)
}

func buildReDocPage(cfg Config, specURL string) string {
	ui := cfg.UI
	opts := marshalOptions(cfg.Options)

	var jsURL string
	if cfg.AssetsPath != "" {
		base := strings.TrimRight(cfg.AssetsPath, "/")
		jsURL = html.EscapeString(base + "/redoc.standalone.js")
	} else {
		jsURL = "https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
</head>
<body>
<div id="redoc-container"></div>
<script src="%s"></script>
<script>
Redoc.init(%s, %s, document.getElementById("redoc-container"));
</script>
</body>
</html>`, html.EscapeString(ui.Title), jsURL, jsString(specURL), opts)
}
