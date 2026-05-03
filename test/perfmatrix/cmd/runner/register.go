package main

// Blank-imports that pull every server package's init() into the
// runner binary. Without these the servers.Registry() is empty because
// Go only runs the init() of packages in the transitive import closure
// of main.
//
// The per-framework packages are compiled independently, so a broken
// framework package would break this list. Wave 2A/2B agents owning
// each framework package keep their sub-packages buildable; when an
// agent's branch is green, the blank-import for their framework is
// uncommented here.

import (
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/celeris"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/chi"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/echo"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/fasthttp"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/fiber"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/gin"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/iris"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/stdhttp"
	// hertz is still under active development by wave 2B; re-enable
	// when servers/hertz builds cleanly.
	// _ "github.com/goceleris/celeris/test/perfmatrix/servers/hertz".
)
