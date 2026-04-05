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
// # URL Parameters
//
// Access matched parameters by name. Type-safe parsing methods are available:
//
//	id := c.Param("id")                 // string
//	n, err := c.ParamInt("id")          // int
//	n64, err := c.ParamInt64("id")      // int64
//
// Query parameters support defaults and multi-values:
//
//	page := c.Query("page")                      // string
//	page := c.QueryDefault("page", "1")           // with default
//	limit := c.QueryInt("limit", 10)              // int with default
//	tags := c.QueryValues("tag")                  // []string
//	all := c.QueryParams()                        // url.Values
//	raw := c.RawQuery()                           // raw query string
//
// # Route Groups
//
//	api := s.Group("/api")
//	api.GET("/items", listItems)
//
// # Middleware
//
// Middleware is provided by the in-tree middleware/ packages (e.g.
// middleware/logger, middleware/recovery). Use Server.Use to register
// middleware globally or per route group.
//
//	import (
//	    "github.com/goceleris/celeris/middleware/logger"
//	    "github.com/goceleris/celeris/middleware/recovery"
//	)
//	s.Use(logger.New(), recovery.New())
//
// To write custom middleware, use the HandlerFunc signature and call
// Context.Next to invoke downstream handlers. Next returns the first
// error from downstream, which middleware can handle or propagate:
//
//	func timing() celeris.HandlerFunc {
//	    return func(c *celeris.Context) error {
//	        err := c.Next()
//	        elapsed := time.Since(c.StartTime())
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
// # Global Error Handler
//
// Register a global error handler with Server.OnError. This is called when
// an unhandled error reaches the safety net after all middleware has had its
// chance. Use it to render structured error responses (e.g. JSON) instead of
// the default text/plain fallback:
//
//	s.OnError(func(c *celeris.Context, err error) {
//	    var he *celeris.HTTPError
//	    code := 500
//	    msg := "internal server error"
//	    if errors.As(err, &he) {
//	        code = he.Code
//	        msg = he.Message
//	    }
//	    c.JSON(code, map[string]string{"error": msg})
//	})
//
// If the handler does not write a response, the default text/plain fallback
// applies. OnError must be called before Start.
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
// values before returning. For the request body specifically, use
// Context.BodyCopy to obtain a copy that outlives the handler:
//
//	safe := c.BodyCopy() // safe to pass to a goroutine
//
// When using Detach, the returned done function MUST be called — failure
// to do so permanently leaks the Context from the pool.
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
// middleware/metrics and middleware/debug packages.
//
// # Configuration
//
// Config.Workers controls the number of I/O workers (default: GOMAXPROCS).
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
// # Static File Serving
//
// Serve an entire directory under a URL prefix:
//
//	s.Static("/assets", "./public")
//
// This is equivalent to:
//
//	s.GET("/assets/*filepath", func(c *celeris.Context) error {
//	    return c.FileFromDir("./public", c.Param("filepath"))
//	})
//
// # Streaming
//
// For simple cases, stream an io.Reader as a buffered response (capped at 100 MB):
//
//	return c.Stream(200, "text/plain", reader)
//
// For true incremental streaming (SSE, chunked responses), use StreamWriter
// with the Detach pattern:
//
//	s.GET("/events", func(c *celeris.Context) error {
//	    sw := c.StreamWriter()
//	    if sw == nil {
//	        return c.String(200, "streaming not supported")
//	    }
//	    done := c.Detach()
//	    sw.WriteHeader(200, [][2]string{{"content-type", "text/event-stream"}})
//	    go func() {
//	        defer done()
//	        defer sw.Close()
//	        for event := range events {
//	            sw.Write([]byte("data: " + event + "\n\n"))
//	            sw.Flush()
//	        }
//	    }()
//	    return nil
//	})
//
// StreamWriter returns nil if the engine does not support streaming.
// The std engine supports streaming; native engines (io_uring, epoll)
// will support it in a future release.
//
// # Cookies
//
// Read and write cookies:
//
//	val, err := c.Cookie("session")
//	c.SetCookie(&celeris.Cookie{Name: "session", Value: token, HTTPOnly: true})
//
// # Authentication
//
// Extract HTTP Basic Authentication credentials:
//
//	user, pass, ok := c.BasicAuth()
//
// # Request Body Parsing
//
// Bind auto-detects the format from Content-Type:
//
//	var user User
//	if err := c.Bind(&user); err != nil {
//	    return err
//	}
//
// Or use format-specific methods:
//
//	c.BindJSON(&user)  // application/json
//	c.BindXML(&user)   // application/xml
//
// For raw body access:
//
//	body := c.Body()         // []byte, valid only during handler
//	safe := c.BodyCopy()     // []byte, safe to retain after handler
//	r := c.BodyReader()      // io.Reader wrapper
//
// # Response Methods
//
// Context provides typed response methods:
//
//	c.JSON(200, data)                      // application/json
//	c.XML(200, data)                       // application/xml
//	c.HTML(200, "<h1>Hello</h1>")          // text/html
//	c.String(200, "Hello, %s", name)       // text/plain (fmt.Sprintf)
//	c.Blob(200, "image/png", pngBytes)     // arbitrary content type
//	c.NoContent(204)                       // status only, no body
//	c.Redirect(302, "/new-location")       // redirect with Location header
//	c.File("/path/to/report.pdf")          // file with MIME detection + Range
//	c.FileFromDir(baseDir, userPath)       // safe file serving (traversal-safe)
//	c.Stream(200, "text/plain", reader)    // io.Reader → response (100 MB cap)
//	c.Respond(200, data)                   // auto-format based on Accept header
//
// All response methods return ErrResponseWritten if called after a response
// has already been sent.
//
// # Content Negotiation
//
// Inspect the Accept header and auto-select the response format:
//
//	best := c.Negotiate("application/json", "application/xml", "text/plain")
//
// Or use Respond to auto-format based on Accept:
//
//	return c.Respond(200, myStruct) // JSON, XML, or text based on Accept
//
// # Accept Negotiation
//
// Beyond content type, negotiate encodings and languages:
//
//	enc := c.AcceptsEncodings("gzip", "br", "identity")
//	lang := c.AcceptsLanguages("en", "fr", "de")
//
// # Route-Level Middleware
//
// Attach middleware to individual routes without creating a group:
//
//	s.GET("/admin", adminHandler).Use(authMiddleware)
//
// Route.Use inserts middleware before the final handler, after server/group
// middleware. Must be called before Server.Start.
//
// # Response Capture
//
// Middleware can inspect the response body after c.Next() by opting in:
//
//	func logger() celeris.HandlerFunc {
//	    return func(c *celeris.Context) error {
//	        c.CaptureResponse()
//	        err := c.Next()
//	        body := c.ResponseBody()       // captured response body
//	        ct := c.ResponseContentType()  // captured Content-Type
//	        // ... log body, ct ...
//	        return err
//	    }
//	}
//
// # Response Buffering
//
// Middleware that needs to transform response bodies (compress, ETag, cache)
// uses BufferResponse to intercept and modify the response before it is sent:
//
//	func compress() celeris.HandlerFunc {
//	    return func(c *celeris.Context) error {
//	        c.BufferResponse()
//	        err := c.Next()
//	        if err != nil {
//	            return err
//	        }
//	        body := c.ResponseBody()
//	        compressed := gzip(body)
//	        c.SetResponseBody(compressed)
//	        c.SetHeader("content-encoding", "gzip")
//	        return c.FlushResponse()
//	    }
//	}
//
// BufferResponse is depth-tracked: multiple middleware layers can each call
// BufferResponse, and the response is only sent when the outermost layer
// calls FlushResponse. If middleware forgets to flush, a safety net in the
// handler adapter auto-flushes the response.
//
// CaptureResponse and BufferResponse serve different purposes. CaptureResponse
// is read-only: the response is written to the wire AND a copy is captured for
// inspection (ideal for loggers). BufferResponse defers the wire write entirely,
// allowing middleware to transform the body before sending (ideal for compress,
// ETag, cache). If both are active, BufferResponse takes precedence.
//
// # Response Inspection
//
// Check response state from middleware after calling c.Next():
//
//	written := c.IsWritten()         // true after response sent to wire
//	size := c.BytesWritten()         // response body size in bytes
//	status := c.StatusCode()         // status code set by handler
//	status := c.ResponseStatus()     // captured status (with BufferResponse)
//	hdrs := c.RequestHeaders()       // all request headers as [][2]string
//
// # Error Types
//
// Handlers signal HTTP errors via HTTPError:
//
//	return celeris.NewHTTPError(404, "user not found")
//	return celeris.NewHTTPError(500, "db error").WithError(err)  // wrap cause
//
// Sentinel errors for common conditions:
//
//	celeris.ErrResponseWritten   // response already sent
//	celeris.ErrEmptyBody         // Bind called with empty body
//	celeris.ErrNoCookie          // Cookie() with missing cookie
//	celeris.ErrHijackNotSupported // Hijack on unsupported connection
//
// # Flow Control
//
// Middleware calls Next to invoke downstream handlers. Abort stops the chain:
//
//	err := c.Next()              // call next handler; returns first error
//	c.Abort()                    // stop chain (does not write response)
//	c.AbortWithStatus(403)       // stop chain and send status code
//	c.IsAborted()                // true if Abort was called
//
// # Key-Value Storage
//
// Store request-scoped data for sharing between middleware and handlers:
//
//	c.Set("userID", 42)
//	id, ok := c.Get("userID")
//	all := c.Keys()              // copy of all stored pairs
//
// # Content-Disposition
//
// Set Content-Disposition headers for file downloads or inline display:
//
//	c.Attachment("report.pdf")   // prompts download
//	c.Inline("image.png")       // suggests inline display
//
// # Request Detection
//
// Detect request characteristics:
//
//	c.IsWebSocket()              // true if Upgrade: websocket
//	c.IsTLS()                    // true if X-Forwarded-Proto is "https"
//
// # Form Presence
//
// FormValueOK distinguishes a missing field from an empty value:
//
//	val, ok := c.FormValueOK("name")
//	if !ok {
//	    // field was not submitted
//	}
//
// # Request Inspection
//
// Additional request accessors:
//
//	scheme := c.Scheme()         // "http" or "https" (checks X-Forwarded-Proto)
//	ip := c.ClientIP()           // from X-Forwarded-For or X-Real-Ip
//	method := c.Method()         // HTTP method
//	path := c.Path()             // path without query string
//	full := c.FullPath()         // matched route pattern (e.g. "/users/:id")
//	raw := c.RawQuery()          // raw query string without leading '?'
//
// # Remote Address
//
// Context.RemoteAddr returns the TCP peer address. On native engines (epoll,
// io_uring), the address is captured from accept(2) or getpeername(2). On the
// std engine, it comes from http.Request.RemoteAddr.
//
//	addr := c.RemoteAddr() // e.g. "192.168.1.1:54321"
//
// # Host
//
// Context.Host returns the request host, checking the :authority pseudo-header
// first (HTTP/2) then falling back to the Host header (HTTP/1.1):
//
//	host := c.Host() // e.g. "example.com"
//
// # Connection Hijacking
//
// HTTP/1.1 connections can be hijacked on all engines for WebSocket or
// other protocols that require raw TCP access:
//
//	conn, err := c.Hijack()
//	if err != nil {
//	    return err
//	}
//	defer conn.Close()
//	// conn is a net.Conn — caller owns the connection
//
// HTTP/2 connections cannot be hijacked because multiplexed streams
// share a single TCP connection.
//
// # Graceful Restart
//
// Start the server with a pre-existing listener for zero-downtime deploys:
//
//	ln, _ := celeris.InheritListener("LISTEN_FD")
//	if ln != nil {
//	    log.Fatal(s.StartWithListener(ln))
//	} else {
//	    log.Fatal(s.Start())
//	}
//
// # Config Surface Area
//
// In addition to the basic config fields, the following tuning fields
// are available:
//
//	celeris.Config{
//	    // Basic
//	    Addr:     ":8080",
//	    Protocol: celeris.Auto,        // HTTP1, H2C, or Auto
//	    Engine:   celeris.Adaptive,    // IOUring, Epoll, Adaptive, Std
//
//	    // Workers
//	    Workers:   0,                  // I/O workers (default GOMAXPROCS)
//
//	    // Timeouts (0 = default: 300s read/write, 600s idle; -1 = no timeout)
//	    ReadTimeout:     0,
//	    WriteTimeout:    0,
//	    IdleTimeout:     0,
//	    ShutdownTimeout: 30*time.Second,
//
//	    // Limits
//	    MaxFormSize: 0,                // multipart form memory (0 = 32 MB; -1 = unlimited)
//	    MaxConns:    0,                // max connections per worker (0 = unlimited)
//
//	    // H2 Tuning
//	    MaxConcurrentStreams: 100,     // H2 streams per connection
//	    MaxFrameSize:        16384,   // H2 frame payload size
//	    InitialWindowSize:   65535,   // H2 stream flow-control window
//	    MaxHeaderBytes:      0,       // header block size (0 = 16 MB)
//
//	    // I/O
//	    DisableKeepAlive: false,       // disable HTTP keep-alive
//	    BufferSize:       0,           // per-connection I/O buffer (0 = engine default)
//	    SocketRecvBuf:    0,           // SO_RCVBUF (0 = OS default)
//	    SocketSendBuf:    0,           // SO_SNDBUF (0 = OS default)
//
//	    // Observability
//	    DisableMetrics: false,         // skip per-request metric recording
//	    Logger: nil,                   // *slog.Logger for engine diagnostics
//
//	    // Connection callbacks (must be fast — blocks the event loop)
//	    OnConnect:    func(addr string) { ... },
//	    OnDisconnect: func(addr string) { ... },
//	}
//
// # Listener Address
//
// After Start or StartWithContext, Server.Addr returns the bound address.
// This is useful when listening on ":0" to discover the OS-assigned port:
//
//	addr := s.Addr() // net.Addr; use addr.String() for "127.0.0.1:49152"
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
