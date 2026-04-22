// Package hertz registers cloudwego/hertz against the perfmatrix
// registry. Three flavours: H1, H2C (via hertz-contrib/http2), Auto.
package hertz

// Wave-2 fills in init() with servers.Register calls for
// "hertz-h1", "hertz-h2c", "hertz-auto".
func init() {}
