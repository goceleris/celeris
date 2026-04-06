package swagger_test

import (
	"fmt"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/swagger"
)

func ExampleNew() {
	spec := []byte(`{"openapi":"3.0.0","info":{"title":"My API","version":"1.0"}}`)
	mw := swagger.New(swagger.Config{
		SpecContent: spec,
	})
	fmt.Printf("middleware type: %T\n", mw)
	// Output: middleware type: celeris.HandlerFunc
}

func ExampleNew_scalar() {
	spec := []byte(`{"openapi":"3.0.0","info":{"title":"My API","version":"1.0"}}`)
	mw := swagger.New(swagger.Config{
		SpecContent: spec,
		UIEngine:    "scalar",
	})
	fmt.Printf("middleware type: %T\n", mw)
	// Output: middleware type: celeris.HandlerFunc
}

func ExampleNew_withAuth() {
	spec := []byte(`{"openapi":"3.0.0","info":{"title":"My API","version":"1.0"}}`)
	mw := swagger.New(swagger.Config{
		SpecContent: spec,
		AuthFunc: func(c *celeris.Context) bool {
			return c.Header("x-api-key") == "secret"
		},
	})
	fmt.Printf("middleware type: %T\n", mw)
	// Output: middleware type: celeris.HandlerFunc
}
