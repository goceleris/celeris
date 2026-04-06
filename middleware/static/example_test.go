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

func ExampleNew_browse() {
	// Enable directory listing.
	_ = static.New(static.Config{
		Root:   "./public",
		Browse: true,
	})
}

func ExampleNew_embedFS() {
	// Serve files from an embedded filesystem.
	_ = static.New(static.Config{
		FS:     embeddedFS,
		Prefix: "/assets",
	})
}
