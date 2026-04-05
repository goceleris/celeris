// Package methodoverride provides HTTP method override middleware for
// celeris.
//
// HTML forms can only submit GET and POST requests. This middleware allows
// clients to tunnel PUT, PATCH, DELETE, and other methods through a POST
// request by specifying the intended method in a header or form field.
//
// # IMPORTANT: Use Server.Pre(), Not Server.Use()
//
// This middleware MUST be registered via [celeris.Server.Pre], not
// [celeris.Server.Use]. With Server.Use, the router has already matched
// the request based on the original method, making the override ineffective.
// Using Server.Use will silently produce wrong routing behavior.
//
// Register the middleware with [celeris.Server.Pre] so the method is
// rewritten before routing:
//
//	s := celeris.New()
//	s.Pre(methodoverride.New())
//
// # Override Sources
//
// By default, the middleware checks the form field [DefaultFormField]
// ("_method") first, then the header [DefaultHeader]
// ("X-HTTP-Method-Override"). Only POST requests are eligible for override
// (configurable via [Config].AllowedMethods).
//
// # Custom Getters
//
// Use [HeaderGetter] to read from a specific header only:
//
//	s.Pre(methodoverride.New(methodoverride.Config{
//	    Getter: methodoverride.HeaderGetter("X-Method"),
//	}))
//
// Use [FormFieldGetter] to read from a specific form field only:
//
//	s.Pre(methodoverride.New(methodoverride.Config{
//	    Getter: methodoverride.FormFieldGetter("_http_method"),
//	}))
//
// Use [FormThenHeaderGetter] to check a custom form field first, then a
// custom header (same order as the default getter but with custom names):
//
//	s.Pre(methodoverride.New(methodoverride.Config{
//	    Getter: methodoverride.FormThenHeaderGetter("_method", "X-HTTP-Method"),
//	}))
//
// # Target Methods
//
// By default, only PUT, DELETE, and PATCH are valid override targets
// (configurable via [Config].TargetMethods). Override values not in this
// list are silently ignored, preventing clients from overriding to
// arbitrary methods such as CONNECT or TRACE.
//
// # Validation
//
// [Config].AllowedMethods and [Config].TargetMethods must not contain empty
// or whitespace-only strings. The middleware panics at initialization if
// this constraint is violated.
//
// # Skipping
//
// Set [Config].Skip to bypass the middleware dynamically, or
// [Config].SkipPaths for exact-match path exclusions.
//
// # CSRF Middleware Interaction
//
// Method override changes the request method before CSRF middleware runs.
// Ensure that overridden methods (PUT, DELETE, PATCH) are NOT in the CSRF
// middleware's SafeMethods list. See middleware/csrf documentation for details.
package methodoverride
