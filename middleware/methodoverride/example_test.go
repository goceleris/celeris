package methodoverride_test

import (
	"github.com/goceleris/celeris/middleware/methodoverride"
)

func ExampleNew() {
	// Register via server.Pre() for pre-routing method override.
	// s := celeris.New()
	// s.Pre(methodoverride.New())
	_ = methodoverride.New()
}

func ExampleNew_headerOnly() {
	// Override from a custom header only.
	_ = methodoverride.New(methodoverride.Config{
		Getter: methodoverride.HeaderGetter("X-Method"),
	})
}

func ExampleNew_formFieldOnly() {
	// Override from a custom form field only.
	_ = methodoverride.New(methodoverride.Config{
		Getter: methodoverride.FormFieldGetter("_http_method"),
	})
}

func ExampleNew_formThenHeader() {
	// Check form field first, then header (custom names).
	_ = methodoverride.New(methodoverride.Config{
		Getter: methodoverride.FormThenHeaderGetter("_method", "X-HTTP-Method"),
	})
}

func ExampleNew_queryGetter() {
	// Read override from ?_method query parameter.
	// Security: query params are embeddable in links; use with caution.
	_ = methodoverride.New(methodoverride.Config{
		Getter: methodoverride.QueryGetter("_method"),
	})
}

func ExampleNew_customTargets() {
	_ = methodoverride.New(methodoverride.Config{
		TargetMethods: []string{"PUT", "DELETE", "PATCH", "GET"},
	})
}
