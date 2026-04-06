package static_test

import (
	"embed"

	"github.com/goceleris/celeris/middleware/static"
)

//go:embed doc.go
var embeddedFS embed.FS

func ExampleNew() {
	// Serve files from an OS directory.
	_ = static.New(static.Config{
		Root: "./public",
	})
}

func ExampleNew_spa() {
	// Single-page application: non-existent paths serve index.html.
	_ = static.New(static.Config{
		Root:  "./dist",
		SPA:   true,
		Index: "index.html",
	})
}

func ExampleNew_embedFS() {
	// Serve files from an embedded filesystem.
	_ = static.New(static.Config{
		Filesystem: embeddedFS,
		Prefix:     "/assets",
	})
}
