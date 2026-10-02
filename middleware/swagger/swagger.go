package swagger

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strconv"
	"strings"

	"github.com/goceleris/celeris"
)

// The UI pages are html/template templates: it is Go's context-aware
// escaper, so every interpolated configuration value is escaped for the
// exact context it lands in — RCDATA for <title>, URL attribute for href /
// src / data-url (percent-normalised, then HTML-escaped; non-http(s)
// schemes are neutralised), plain attribute for data-configuration and
// integrity, and a JSON string literal (with '<', '>', '&', U+2028 and
// U+2029 escaped, so it cannot close the enclosing <script> block) for
// values inside <script>.
// Bool and int literals are passed as template.JS so html/template does
// not pad them with spaces; they are never configuration strings.
var (
	swaggerUITmpl = template.Must(template.New("swagger-ui").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="{{.CSS.URL}}"{{if .CSS.Integrity}} integrity="{{.CSS.Integrity}}" crossorigin="anonymous"{{end}}>
</head>
<body>
<div id="swagger-ui"></div>
<script src="{{.Bundle.URL}}"{{if .Bundle.Integrity}} integrity="{{.Bundle.Integrity}}" crossorigin="anonymous"{{end}}></script>
<script src="{{.Preset.URL}}"{{if .Preset.Integrity}} integrity="{{.Preset.Integrity}}" crossorigin="anonymous"{{end}}></script>
<script>
const ui = SwaggerUIBundle({
  url: {{.SpecURL}},
  dom_id: "#swagger-ui",
  presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
  layout: "StandaloneLayout",
  docExpansion: {{.DocExpansion}},
  deepLinking: {{.DeepLinking}},
  persistAuthorization: {{.PersistAuthorization}},
  defaultModelsExpandDepth: {{.DefaultModelsExpandDepth}}{{if .OAuth2RedirectURL}},
  oauth2RedirectUrl: {{.OAuth2RedirectURL}}{{end}}
});{{if .InitOAuth}}
ui.initOAuth({ {{- range $i, $p := .OAuth2}}{{if $i}}, {{end}}{{$p.Key}}: {{$p.Value}}{{end}}});{{end}}
</script>
</body>
</html>`))

	scalarTmpl = template.Must(template.New("scalar").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
</head>
<body>
<script id="api-reference" data-url="{{.SpecURL}}" data-configuration='{{.Configuration}}'></script>
<script src="{{.Script.URL}}"{{if .Script.Integrity}} integrity="{{.Script.Integrity}}" crossorigin="anonymous"{{end}}></script>
</body>
</html>`))

	redocTmpl = template.Must(template.New("redoc").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
</head>
<body>
<div id="redoc-container"></div>
<script src="{{.Script.URL}}"{{if .Script.Integrity}} integrity="{{.Script.Integrity}}" crossorigin="anonymous"{{end}}></script>
<script>
Redoc.init({{.SpecURL}}, {{.Options}}, document.getElementById("redoc-container"));
</script>
</body>
</html>`))
)

// jsProp is one property of the ui.initOAuth({...}) object literal. Key is
// a constant JavaScript identifier chosen by this package, never
// configuration, hence template.JS; Value is either a configuration string
// (escaped by html/template as a JS string literal) or a template.JS
// literal.
type jsProp struct {
	Key   template.JS
	Value any
}

// swaggerUIPage is the data for swaggerUITmpl.
type swaggerUIPage struct {
	Title                    string
	CSS                      assetRef
	Bundle                   assetRef
	Preset                   assetRef
	SpecURL                  string
	DocExpansion             string
	DeepLinking              template.JS
	PersistAuthorization     template.JS
	DefaultModelsExpandDepth template.JS
	OAuth2RedirectURL        string
	InitOAuth                bool
	OAuth2                   []jsProp
}

// scalarPage is the data for scalarTmpl.
type scalarPage struct {
	Title         string
	SpecURL       string
	Configuration string
	Script        assetRef
}

// redocPage is the data for redocTmpl.
type redocPage struct {
	Title   string
	Script  assetRef
	SpecURL string
	Options map[string]any
}

// marshalOptions serializes the Options map to a JSON string for embedding
// in an HTML attribute. Returns "{}" when opts is nil or empty.
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

// renderPage executes tmpl with data. Every field is a plain string, a
// bool/int literal or an Options map that validate() has already proven
// JSON-serializable, so Execute cannot fail; a failure is a template bug
// and New() is startup code.
func renderPage(tmpl *template.Template, data any) string {
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		panic(fmt.Sprintf("swagger: render %s page: %v", tmpl.Name(), err))
	}
	return b.String()
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

	page := buildPage(cfg, basePath, specURL)

	// The embedded Swagger UI files are served only when the page loads
	// them; with AssetsPath or CDN their paths pass through.
	var assets map[string]embeddedAsset
	if cfg.Renderer == RendererSwaggerUI && cfg.AssetsPath == "" && !cfg.CDN {
		assets = embeddedAssetRoutes(basePath)
	}

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()

		// Every path this middleware answers starts with basePath, so
		// one prefix test lets all other requests through.
		if !strings.HasPrefix(path, basePath) {
			return c.Next()
		}

		var asset embeddedAsset
		if path != basePath && path != uiPath && path != specPath {
			var ok bool
			if asset, ok = assets[path]; !ok {
				return c.Next()
			}
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

		c.SetHeader("cache-control", assetCacheControl)
		return c.Blob(200, asset.contentType, asset.body)
	}
}

// buildPage generates the HTML page for the configured renderer.
func buildPage(cfg Config, basePath, specURL string) string {
	switch cfg.Renderer {
	case RendererScalar:
		return buildScalarPage(cfg, specURL)
	case RendererReDoc:
		return buildReDocPage(cfg, specURL)
	default:
		return buildSwaggerUIPage(cfg, basePath, specURL)
	}
}

func buildSwaggerUIPage(cfg Config, basePath, specURL string) string {
	ui := cfg.UI

	depth := 1 // Swagger UI default
	if ui.DefaultModelsExpandDepth != nil {
		depth = *ui.DefaultModelsExpandDepth
	}

	css, bundle, preset := swaggerUIRefs(cfg, basePath)

	data := swaggerUIPage{
		Title:                    ui.Title,
		CSS:                      css,
		Bundle:                   bundle,
		Preset:                   preset,
		SpecURL:                  specURL,
		DocExpansion:             ui.DocExpansion,
		DeepLinking:              template.JS(strconv.FormatBool(ui.DeepLinking)),
		PersistAuthorization:     template.JS(strconv.FormatBool(ui.PersistAuthorization)),
		DefaultModelsExpandDepth: template.JS(strconv.Itoa(depth)),
		OAuth2RedirectURL:        ui.OAuth2RedirectURL,
	}

	if oa := ui.OAuth2; oa != nil {
		data.InitOAuth = true
		if oa.ClientID != "" {
			data.OAuth2 = append(data.OAuth2, jsProp{"clientId", oa.ClientID})
		}
		if oa.UsePKCE {
			data.OAuth2 = append(data.OAuth2, jsProp{"usePkceWithAuthorizationCodeGrant", template.JS("true")})
		}
		if oa.Realm != "" {
			data.OAuth2 = append(data.OAuth2, jsProp{"realm", oa.Realm})
		}
		if oa.AppName != "" {
			data.OAuth2 = append(data.OAuth2, jsProp{"appName", oa.AppName})
		}
		if len(oa.Scopes) > 0 {
			data.OAuth2 = append(data.OAuth2, jsProp{"scopes", strings.Join(oa.Scopes, " ")})
		}
	}

	return renderPage(swaggerUITmpl, data)
}

func buildScalarPage(cfg Config, specURL string) string {
	scalarOpts := cfg.Options
	if scalarOpts == nil {
		scalarOpts = map[string]any{"theme": "default"}
	}

	return renderPage(scalarTmpl, scalarPage{
		Title:         cfg.UI.Title,
		SpecURL:       specURL,
		Configuration: marshalOptions(scalarOpts),
		Script:        scalarRef(cfg),
	})
}

func buildReDocPage(cfg Config, specURL string) string {
	// html/template marshals the map itself as the JS object literal; an
	// empty map (rather than nil) keeps the "{}" default.
	opts := cfg.Options
	if len(opts) == 0 {
		opts = map[string]any{}
	}

	return renderPage(redocTmpl, redocPage{
		Title:   cfg.UI.Title,
		Script:  redocRef(cfg),
		SpecURL: specURL,
		Options: opts,
	})
}
