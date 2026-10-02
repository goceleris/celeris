// Package swagger provides OpenAPI specification viewer middleware for celeris.
//
// [New] returns a [celeris.HandlerFunc] that serves an interactive API
// reference page and the raw OpenAPI spec (JSON or YAML, auto-detected) under
// a configurable base path. By default it registers {BasePath}/ for the UI,
// {BasePath}/spec for the raw spec, and redirects {BasePath} to {BasePath}/;
// other paths and non-GET/HEAD methods pass through or return 405.
//
// [Config] is the entry point. Provide the spec via Config.SpecContent (an
// embedded byte slice) or Config.SpecURL (an externally hosted spec, in which
// case no /spec endpoint is registered). Config.Renderer selects the frontend
// ([RendererSwaggerUI] (default), [RendererScalar], or [RendererReDoc]), and
// Config.Options passes renderer-specific settings as a JSON-serializable map.
//
// [UIConfig] (Config.UI) tunes the Swagger UI renderer — DocExpansion, Title,
// and OAuth2 pre-configuration via [OAuth2Config]; most of its fields are
// ignored by Scalar and ReDoc. Use [IntPtr] to set UIConfig.DefaultModelsExpandDepth,
// which is an *int so an explicit zero is distinguishable from unset.
//
// # Assets
//
// Swagger UI is embedded in the package (swagger-ui-dist [SwaggerUIVersion],
// with its upstream licence notices) and served by default under
// {BasePath}/assets/, so the page works offline and needs no third-party
// origin in its Content-Security-Policy. Config.CDN opts into loading the
// renderer from jsDelivr instead, pinned to an exact version with a
// Subresource Integrity hash; Config.AssetsPath points the page at files you
// serve yourself. Scalar and ReDoc are not embedded, to keep their bundles
// out of every binary that imports this package: they need Config.CDN or
// Config.AssetsPath, and [New] panics without one.
//
// The middleware has no built-in authentication; OpenAPI specs may expose
// internal API structure, so place it after auth middleware to protect the
// endpoints.
//
//	//go:embed openapi.json
//	var spec []byte
//
//	server.Use(swagger.New(swagger.Config{SpecContent: spec}))
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-content
package swagger
