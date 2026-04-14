package protobuf_test

import (
	"fmt"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/protobuf"
)

// Use Respond when you want a single handler to serve protobuf to
// protobuf-aware clients and JSON to everyone else. The Accept header
// is parsed for q-values and the highest-ranked match wins.
func ExampleRespond() {
	s := celeris.New(celeris.Config{})

	s.GET("/profile", func(c *celeris.Context) error {
		// Protobuf message; JSON fallback uses the same data via map.
		msg := wrapperspb.String("alice")
		jsonFallback := map[string]string{"name": "alice"}
		return protobuf.Respond(c, 200, msg, jsonFallback)
	})

	fmt.Println("ok")
	// Output: ok
}

// Install the middleware to add a c.Get("protobuf.config") helper that
// other middleware (e.g. logger) can use to detect protobuf responses.
func ExampleNew() {
	s := celeris.New(celeris.Config{})
	s.Use(protobuf.New())

	s.POST("/echo", func(c *celeris.Context) error {
		var in wrapperspb.StringValue
		if err := protobuf.BindProtoBuf(c, &in); err != nil {
			return celeris.NewHTTPError(400, "invalid protobuf body")
		}
		return protobuf.Write(c, 200, &in)
	})

	fmt.Println("ok")
	// Output: ok
}
