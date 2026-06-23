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
// which is an *int so an explicit zero is distinguishable from unset. For
// air-gapped deployments, set Config.AssetsPath to serve bundled assets locally
// instead of from a CDN.
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
