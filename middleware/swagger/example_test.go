package swagger_test

import (
	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/swagger"
)

func ExampleNew() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
	}))
}

func ExampleNew_customUI() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		BasePath:    "/docs",
		UI: swagger.UIConfig{
			DocExpansion:         "full",
			DeepLinking:          true,
			PersistAuthorization: true,
			Title:                "My API",
		},
	}))
}

func ExampleNew_scalar() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		Renderer:    swagger.RendererScalar,
	}))
}

func ExampleNew_externalSpec() {
	server := celeris.New(celeris.Config{})

	server.Use(swagger.New(swagger.Config{
		SpecURL: "https://petstore.swagger.io/v2/swagger.json",
	}))
}

func ExampleNew_redoc() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		Renderer:    swagger.RendererReDoc,
	}))
}

func ExampleNew_localAssets() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		AssetsPath:  "/swagger-assets",
	}))
}

func ExampleNew_oauth2() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		UI: swagger.UIConfig{
			OAuth2RedirectURL: "https://example.com/oauth2-redirect",
			OAuth2: &swagger.OAuth2Config{
				ClientID: "my-client-id",
				AppName:  "My Application",
				Scopes:   []string{"read:api", "write:api"},
			},
		},
	}))
}

func ExampleNew_redocCustom() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		Renderer:    swagger.RendererReDoc,
		ReDoc: swagger.ReDocConfig{
			Theme:              "dark",
			ExpandResponses:    "200",
			HideDownloadButton: true,
		},
	}))
}

func ExampleIntPtr() {
	server := celeris.New(celeris.Config{})

	spec := []byte(`{"openapi":"3.0.0","info":{"title":"Test","version":"1.0"}}`)

	server.Use(swagger.New(swagger.Config{
		SpecContent: spec,
		UI: swagger.UIConfig{
			DefaultModelsExpandDepth: swagger.IntPtr(0),
		},
	}))
}
