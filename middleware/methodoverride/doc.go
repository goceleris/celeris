// Package methodoverride provides HTTP method override middleware for celeris.
//
// HTML forms can only submit GET and POST requests. This middleware lets
// clients tunnel PUT, PATCH, DELETE, and other methods through a POST request
// by specifying the intended method via a form field or header.
//
// Register with [celeris.Server.Pre] (not Server.Use) so the method is
// rewritten before routing occurs. [New] accepts an optional [Config] to
// customise behaviour:
//
//   - [Config].AllowedMethods — original methods eligible for override
//     (default: POST).
//   - [Config].TargetMethods — valid override targets (default: PUT, DELETE,
//     PATCH); values outside this set are silently ignored.
//   - [Config].Getter — function that extracts the override value; built-in
//     helpers are [HeaderGetter], [FormFieldGetter], [FormThenHeaderGetter],
//     and [QueryGetter] (note: QueryGetter carries CSRF risk via embeddable
//     URLs).
//   - [Config].Skip / [Config].SkipPaths — skip the middleware per-request or
//     by exact path.
//
// The default getter checks form field [DefaultFormField] ("_method") first,
// then header [DefaultHeader] ("X-HTTP-Method-Override"). [Config].AllowedMethods
// and [Config].TargetMethods must not contain empty or whitespace-only strings;
// the middleware panics at initialisation if this constraint is violated.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-routing-helpers
package methodoverride
