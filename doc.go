// Package celeris provides an ultra-low latency HTTP server with
// dual-architecture I/O (io_uring + epoll) and a high-level API
// for routing and request handling.
//
// # Quick Start
//
//	s := celeris.New(celeris.Config{Addr: ":8080"})
//	s.GET("/hello", func(c *celeris.Context) error {
//	    return c.String(200, "Hello, World!")
//	})
//	log.Fatal(s.Start())
//
// # Routing
//
// Routes support static paths, named parameters, and catch-all wildcards:
//
//	s.GET("/users/:id", handler)     // /users/42 → Param("id") = "42"
//	s.GET("/files/*path", handler)   // /files/a/b → Param("path") = "/a/b"
//
// # Route Groups
//
//	api := s.Group("/api")
//	api.GET("/items", listItems)
//
// # Middleware
//
// Middleware is provided by the github.com/goceleris/middlewares module.
// Use Server.Use to register middleware globally or per route group.
//
//	s.Use(middlewares.Logger(), middlewares.Recovery())
//
// To write custom middleware, use the HandlerFunc signature and call
// Context.Next to invoke downstream handlers. Next returns the first
// error from downstream, which middleware can handle or propagate:
//
//	func timing() celeris.HandlerFunc {
//	    return func(c *celeris.Context) error {
//	        start := time.Now()
//	        err := c.Next()
//	        elapsed := time.Since(start)
//	        c.SetHeader("x-response-time", elapsed.String())
//	        return err
//	    }
//	}
//
// # Error Handling
//
// Handlers return errors. Unhandled errors are caught by the routerAdapter
// safety net: *HTTPError writes its Code+Message; bare errors write 500.
//
//	s.GET("/data", func(c *celeris.Context) error {
//	    data, err := fetchData()
//	    if err != nil {
//	        return celeris.NewHTTPError(500, "fetch failed")
//	    }
//	    return c.JSON(200, data)
//	})
//
// Middleware can intercept errors from downstream handlers:
//
//	s.Use(func(c *celeris.Context) error {
//	    err := c.Next()
//	    if err != nil {
//	        log.Println("error:", err)
//	        return c.JSON(500, map[string]string{"error": "internal"})
//	    }
//	    return nil
//	})
//
// # Custom 404 / 405 Handlers
//
//	s.NotFound(func(c *celeris.Context) error {
//	    return c.JSON(404, map[string]string{"error": "not found"})
//	})
//	s.MethodNotAllowed(func(c *celeris.Context) error {
//	    return c.JSON(405, map[string]string{"error": "method not allowed"})
//	})
//
// # Graceful Shutdown
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
//	defer stop()
//
//	s := celeris.New(celeris.Config{
//	    Addr:            ":8080",
//	    ShutdownTimeout: 10 * time.Second,
//	})
//	s.GET("/ping", func(c *celeris.Context) error {
//	    return c.String(200, "pong")
//	})
//	if err := s.StartWithContext(ctx); err != nil {
//	    log.Fatal(err)
//	}
//
// # Engine Selection
//
// On Linux, choose between IOUring, Epoll, Adaptive, or Std engines.
// On other platforms, only Std is available.
//
//	s := celeris.New(celeris.Config{
//	    Addr:   ":8080",
//	    Engine: celeris.Adaptive,
//	})
//
// # Protocol Selection
//
// The Protocol field controls HTTP version negotiation:
//
//	celeris.HTTP1    // HTTP/1.1 only (default)
//	celeris.H2C      // HTTP/2 cleartext (h2c) only
//	celeris.Auto     // Auto-detect: serves both HTTP/1.1 and H2C
//
// Example:
//
//	s := celeris.New(celeris.Config{
//	    Addr:     ":8080",
//	    Protocol: celeris.Auto,
//	})
//
// # net/http Compatibility
//
// Wrap existing net/http handlers. Response bodies from adapted handlers
// are buffered in memory (capped at 100 MB).
//
//	s.GET("/legacy", celeris.Adapt(legacyHandler))
//
// # Context Lifecycle
//
// Context objects are pooled and recycled between requests. Do not retain
// references to a *[Context] after the handler returns. Copy any needed
// values before returning.
//
// # Observability
//
// The Server.Collector method returns an [observe.Collector] that records
// per-request metrics (throughput, latency histogram, error rate, active
// connections). Use Collector.Snapshot to retrieve a point-in-time copy:
//
//	snap := s.Collector().Snapshot()
//	fmt.Println(snap.RequestsTotal, snap.ErrorsTotal)
//
// For Prometheus or debug endpoint integration, see the
// github.com/goceleris/middlewares module.
//
// # Configuration
//
// Config.Workers controls the number of I/O workers (default: GOMAXPROCS).
// Config.Objective selects a tuning profile:
//
//	celeris.Latency     // Optimize for minimum response time
//	celeris.Throughput  // Optimize for maximum requests per second
//	celeris.Balanced    // Balance between latency and throughput (default)
//
// Config.ShutdownTimeout sets the graceful shutdown deadline for
// StartWithContext (default: 30s).
//
// # Named Routes & Reverse URLs
//
// Assign names to routes with Route.Name, then generate URLs via Server.URL:
//
//	s.GET("/users/:id", handler).Name("user")
//	url, _ := s.URL("user", "42") // "/users/42"
//
// For catch-all routes the value replaces the wildcard segment:
//
//	s.GET("/files/*filepath", handler).Name("files")
//	url, _ := s.URL("files", "/css/style.css") // "/files/css/style.css"
//
// Use Server.Routes to list all registered routes.
//
// # Form Handling
//
// Parse url-encoded and multipart form bodies:
//
//	name := c.FormValue("name")
//	all  := c.FormValues("tags")
//
// For file uploads, use FormFile or MultipartForm:
//
//	file, header, err := c.FormFile("avatar")
//	defer file.Close()
//
// # File Serving
//
// Serve static files with automatic content-type detection and Range support:
//
//	s.GET("/download", func(c *celeris.Context) error {
//	    return c.File("/var/data/report.pdf")
//	})
//
// Callers must sanitize user-supplied paths to prevent directory traversal.
//
// # Streaming
//
// Stream an io.Reader as the response body (capped at 100 MB):
//
//	return c.Stream(200, "text/plain", reader)
//
// # Cookies
//
// Read and write cookies:
//
//	val := c.Cookie("session")
//	c.SetCookie(celeris.Cookie{Name: "session", Value: token, HTTPOnly: true})
//
// # Authentication
//
// Extract HTTP Basic Authentication credentials:
//
//	user, pass, ok := c.BasicAuth()
//
// # Listener Address
//
// After Start or StartWithContext, Server.Addr returns the bound address.
// This is useful when listening on ":0" to discover the OS-assigned port:
//
//	addr := s.Addr() // e.g. 127.0.0.1:49152
//
// # Testing
//
// The github.com/goceleris/celeris/celeristest package provides test helpers:
//
//	ctx, rec := celeristest.NewContext("GET", "/hello")
//	defer celeristest.ReleaseContext(ctx)
//	handler(ctx)
//	// inspect rec.StatusCode, rec.Headers, rec.Body
package celeris
