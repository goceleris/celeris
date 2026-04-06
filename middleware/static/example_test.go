package static_test

import (
	"embed"
	"time"

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

func ExampleNew_spa() {
	// Serve a single-page application: non-existent paths fall back to
	// index.html so the client-side router can handle them.
	_ = static.New(static.Config{
		Root: "./dist",
		SPA:  true,
	})
}

func ExampleNew_maxAge() {
	// Set a 24-hour Cache-Control max-age alongside the automatic ETag
	// and Last-Modified headers.
	_ = static.New(static.Config{
		Root:   "./public",
		MaxAge: 24 * time.Hour,
	})
}

func ExampleNew_compress() {
	// Serve pre-compressed files when available. If the client sends
	// Accept-Encoding: br, the middleware serves app.js.br instead of
	// app.js (and sets Content-Encoding: br). Falls back to the original
	// file when no pre-compressed variant exists.
	_ = static.New(static.Config{
		Root:     "./dist",
		Compress: true,
		MaxAge:   7 * 24 * time.Hour,
	})
}
