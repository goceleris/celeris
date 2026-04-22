// Package stdhttp registers the standard library net/http server
// against the perfmatrix registry in three flavours: H1, H2C, and Auto
// (H1+H2C on a single listener using h2c.NewHandler).
package stdhttp

// Wave-2 fills in init() with servers.Register calls for
// "stdhttp-h1", "stdhttp-h2c", "stdhttp-auto".
func init() {}
