// Package swagger provides OpenAPI specification viewer middleware for
// celeris.
//
// The middleware serves a Swagger UI (or Scalar) page and the raw OpenAPI
// spec at configurable URL paths. It supports both JSON and YAML specs
// with automatic content-type detection.
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
//	        DocExpansion:         "full",
//	        DeepLinking:          true,
//	        PersistAuthorization: true,
//	        Title:                "My API",
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
package swagger
