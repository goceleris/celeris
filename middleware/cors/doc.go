// Package cors provides Cross-Origin Resource Sharing (CORS) middleware
// for Celeris.
//
// Call [New] with an optional [Config] to create a middleware handler.
// With no arguments, all origins are allowed (AllowOrigins: ["*"]).
// Pass a Config to restrict origins, enable credentials, set MaxAge, and more.
//
// Key Config fields:
//   - AllowOrigins: static origin list; supports subdomain wildcards ("https://*.example.com").
//   - AllowOriginsFunc: custom callback to validate an origin string.
//   - AllowOriginRequestFunc: like AllowOriginsFunc but receives the full request context.
//   - AllowCredentials: set true to emit Access-Control-Allow-Credentials.
//   - MirrorRequestHeaders: reflect Access-Control-Request-Headers back on preflight.
//   - AllowPrivateNetwork: emit Access-Control-Allow-Private-Network on preflight.
//   - Skip / SkipPaths: bypass CORS processing for selected requests or paths.
//
// AllowOriginsFunc and AllowOriginRequestFunc cannot be combined with a wildcard
// ("*") AllowOrigins entry. Using AllowCredentials with "*" panics at New.
// Subdomain wildcards with AllowCredentials also panic unless
// UnsafeAllowCredentialsWithWildcard is set.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-security
package cors
