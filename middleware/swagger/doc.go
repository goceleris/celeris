// Package swagger provides OpenAPI specification viewer middleware for
// celeris.
//
// The middleware serves a Swagger UI (or Scalar / ReDoc) page and the raw
// OpenAPI spec at configurable URL paths. It supports both JSON and YAML
// specs with automatic content-type detection.
//
// Basic usage with an embedded spec:
//
//	import "embed"
//
//	//go:embed openapi.json
//	var spec []byte
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	}))
//
// Custom base path and UI options:
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    BasePath:    "/docs",
//	    UI: swagger.UIConfig{
//	        DocExpansion:             "full",
//	        DeepLinking:              true,
//	        PersistAuthorization:     true,
//	        DefaultModelsExpandDepth: 1,
//	        Title:                    "My API",
//	    },
//	}))
//
// Using Scalar instead of Swagger UI:
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    Renderer:    swagger.RendererScalar,
//	}))
//
// Using ReDoc instead of Swagger UI:
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    Renderer:    swagger.RendererReDoc,
//	}))
//
// Note that [UIConfig] options (DocExpansion, DeepLinking, PersistAuthorization,
// DefaultModelsExpandDepth) apply only to Swagger UI and are ignored when
// Renderer is Scalar or ReDoc.
//
// # Renderer-Specific Options
//
// Use [Config].Options to pass renderer-specific configuration as a
// JSON-serializable map. For ReDoc these are passed to Redoc.init(),
// for Scalar they become the data-configuration attribute, and for
// Swagger UI they are passed to SwaggerUIBundle().
//
// ReDoc dark theme example:
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    Renderer:    swagger.RendererReDoc,
//	    Options: map[string]any{
//	        "theme": map[string]any{
//	            "colors": map[string]any{"primary": map[string]any{"main": "#32329f"}},
//	        },
//	        "expandResponses":    "200,201",
//	        "hideDownloadButton": true,
//	    },
//	}))
//
// When Options is nil, each renderer uses its own defaults.
//
// # OAuth2 Pre-Configuration
//
// Swagger UI supports pre-filling the OAuth2 authorization dialog. Set
// [UIConfig].OAuth2 to configure client credentials:
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    UI: swagger.UIConfig{
//	        OAuth2RedirectURL: "https://example.com/oauth2-redirect",
//	        OAuth2: &swagger.OAuth2Config{
//	            ClientID: "my-client-id",
//	            AppName:  "My Application",
//	            Scopes:   []string{"read:api", "write:api"},
//	        },
//	    },
//	}))
//
// WARNING: all [OAuth2Config] values including ClientSecret are embedded in
// the HTML page source and visible to anyone who can access the page. Only
// use ClientSecret for development or test environments. In production, use
// PKCE (public clients) which do not require a secret.
//
// OAuth2 fields are ignored when Renderer is not [RendererSwaggerUI].
//
// External spec URL (no /spec endpoint registered):
//
//	server.Use(swagger.New(swagger.Config{
//	    SpecURL: "https://petstore.swagger.io/v2/swagger.json",
//	}))
//
// # Air-Gapped / Self-Hosted Assets
//
// By default, CSS and JavaScript are loaded from public CDNs. For
// environments without internet access, set [Config].AssetsPath to a
// local URL prefix and serve the bundled assets with a static file
// middleware:
//
//	server.Use(static.New(static.Config{
//	    Root:   "./swagger-ui-dist",
//	    Prefix: "/swagger-assets",
//	}))
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    AssetsPath:  "/swagger-assets",
//	}))
//
// # Content-Type Detection
//
// When serving the spec via the /spec endpoint, the middleware
// auto-detects whether the content is JSON or YAML. Detection checks
// the [Config].SpecFile extension first (.json, .yaml, .yml), then
// inspects the first non-whitespace byte of the content ('{' or '['
// indicates JSON; anything else is assumed YAML).
//
// # Endpoints
//
// The middleware registers:
//
//   - {BasePath}/     — renders the UI page (HTML)
//   - {BasePath}/spec — serves the raw spec (JSON or YAML)
//   - {BasePath}      — redirects to {BasePath}/
//
// Requests to other paths pass through. Only GET and HEAD are handled;
// other methods return 405.
//
// # Ordering
//
// The swagger middleware intercepts by path prefix and can be placed at any
// position in the [celeris.Server.Use] chain. Place it after authentication
// middleware if you want to protect the spec and UI endpoints.
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// exact-match path exclusions. Skipped requests call c.Next() without
// serving the UI or spec.
//
// # Security
//
// The middleware has no built-in authentication. OpenAPI specs may reveal
// internal API structure. Protect the endpoints with upstream auth middleware
// or network-level controls. When using CDN-loaded assets, ensure your CSP
// policy allows cdn.jsdelivr.net (Scalar) or unpkg.com (Swagger UI) in
// script-src and style-src.
package swagger
