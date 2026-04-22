// Package chi registers go-chi/chi on top of net/http against the
// perfmatrix registry. Three flavours: H1, H2C, Auto.
package chi

// Wave-2 fills in init() with servers.Register calls for
// "chi-h1", "chi-h2c", "chi-auto".
func init() {}
