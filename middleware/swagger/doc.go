// Package swagger provides API documentation middleware for celeris.
//
// The middleware serves an interactive API documentation UI (Swagger UI
// or Scalar) and the raw OpenAPI specification at configurable URL paths.
// No assets are bundled; both UIs are loaded from CDN (jsdelivr.net) at
// page load time. The HTML pages and spec bytes are rendered once at
// init time and served from memory on each request.
//
// Basic usage with inline spec:
//
//	spec, _ := os.ReadFile("openapi.json")
//	server.Use(swagger.New(swagger.Config{
//	    SpecContent: spec,
//	}))
//
// Usage with embed.FS:
//
//	//go:embed openapi.json
//	var specFS embed.FS
//
//	server.Use(swagger.New(swagger.Config{
//	    Filesystem: specFS,
//	    SpecFile:   "openapi.json",
//	}))
//
// # UI Engines
//
// Set [Config].UIEngine to select the documentation frontend:
//
//   - "swagger-ui" (default): classic Swagger UI from swagger-ui-dist
//   - "scalar": modern Scalar API Reference
//
// # Authentication
//
// Use [Config].AuthFunc to restrict access to documentation endpoints.
// When AuthFunc returns false, the middleware responds with 403 Forbidden:
//
//	swagger.New(swagger.Config{
//	    SpecContent: spec,
//	    AuthFunc: func(c *celeris.Context) bool {
//	        return c.Header("x-api-key") == "secret"
//	    },
//	})
//
// # Content Security Policy (CSP)
//
// The UI pages load JavaScript and CSS from cdn.jsdelivr.net. If the
// secure middleware (or any upstream proxy) sets a Content-Security-Policy
// header, the swagger UI will be blocked unless the CSP includes:
//
//	script-src cdn.jsdelivr.net; style-src cdn.jsdelivr.net;
//
// The CDN URLs use @latest version tags. For production deployments that
// require deterministic builds, provide a custom HTML page via a separate
// static middleware instead of relying on CDN @latest.
//
// # Middleware Ordering
//
// Place swagger after debug middleware and before application-level
// middleware (cors, secure, etc.) so that documentation endpoints are
// served without unnecessary processing:
//
//	server.Use(debug.New(...))
//	server.Use(swagger.New(...))
//	server.Use(cors.New(...))
//
// # fs.FS Support
//
// When [Config].Filesystem is set, the spec is read once during
// middleware initialization and cached. The filesystem is not accessed
// at request time. [Config].SpecFile controls the path within the
// filesystem (default "openapi.json").
//
// # Paths
//
// Default paths: UI at "/swagger", spec at "/swagger/doc.json".
// Both are configurable via [Config].UIPath and [Config].SpecURL.
// Non-matching requests pass through to the next handler with zero
// overhead.
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// path exclusions. SkipPaths uses exact path matching.
package swagger
