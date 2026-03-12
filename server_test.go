package celeris

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

func TestServerEngineInfo(t *testing.T) {
	s := New(Config{})

	// Not started — should be nil.
	if s.EngineInfo() != nil {
		t.Fatal("expected nil EngineInfo before start")
	}

	// Simulate engine set.
	s.engine = &fakeEngine{}
	info := s.EngineInfo()
	if info == nil {
		t.Fatal("expected non-nil EngineInfo")
	}
	if info.Type != Std {
		t.Fatalf("expected Std, got %v", info.Type)
	}
}

func TestNewServer(t *testing.T) {
	s := New(Config{Addr: ":9090"})
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.config.Addr != ":9090" {
		t.Fatalf("expected :9090, got %s", s.config.Addr)
	}
}

func TestServerRouting(t *testing.T) {
	s := New(Config{})
	s.GET("/hello", func(c *Context) error {
		return c.String(200, "hello")
	})
	s.POST("/echo", func(c *Context) error {
		return c.Blob(200, "text/plain", c.Body())
	})

	adapter := &routerAdapter{server: s}

	// Test GET /hello.
	st, rw := newTestStream("GET", "/hello")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "hello" {
		t.Fatalf("expected 200 hello, got %d %s", rw.status, string(rw.body))
	}
	st.Release()

	// Test POST /echo.
	st2, rw2 := newTestStream("POST", "/echo")
	st2.Data.Write([]byte("payload"))
	if err := adapter.HandleStream(context.Background(), st2); err != nil {
		t.Fatal(err)
	}
	if rw2.status != 200 || string(rw2.body) != "payload" {
		t.Fatalf("expected 200 payload, got %d %s", rw2.status, string(rw2.body))
	}
	st2.Release()
}

func TestServerNotFound(t *testing.T) {
	s := New(Config{})
	s.GET("/exists", func(c *Context) error {
		return c.String(200, "ok")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/missing")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 404 {
		t.Fatalf("expected 404, got %d", rw.status)
	}
	st.Release()
}

func TestServerMiddleware(t *testing.T) {
	s := New(Config{})
	order := ""
	s.Use(func(c *Context) error {
		order += "A"
		_ = c.Next()
		order += "C"
		return nil
	})
	s.GET("/test", func(c *Context) error {
		order += "B"
		return c.String(200, "ok")
	})

	adapter := &routerAdapter{server: s}

	st, _ := newTestStream("GET", "/test")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if order != "ABC" {
		t.Fatalf("expected ABC, got %s", order)
	}
	st.Release()
}

func TestServerGroups(t *testing.T) {
	s := New(Config{})

	api := s.Group("/api")
	api.GET("/users", func(c *Context) error {
		return c.String(200, "users")
	})

	v2 := api.Group("/v2")
	v2.GET("/items", func(c *Context) error {
		return c.String(200, "v2 items")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/api/users")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "users" {
		t.Fatalf("expected 200 users, got %d %s", rw.status, string(rw.body))
	}
	st.Release()

	st2, rw2 := newTestStream("GET", "/api/v2/items")
	if err := adapter.HandleStream(context.Background(), st2); err != nil {
		t.Fatal(err)
	}
	if rw2.status != 200 || string(rw2.body) != "v2 items" {
		t.Fatalf("expected 200 v2 items, got %d %s", rw2.status, string(rw2.body))
	}
	st2.Release()
}

func TestServerParamsViaHandler(t *testing.T) {
	s := New(Config{})
	s.GET("/users/:id", func(c *Context) error {
		return c.String(200, "user-%s", c.Param("id"))
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/users/42")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "user-42" {
		t.Fatalf("expected 200 user-42, got %d %s", rw.status, string(rw.body))
	}
	st.Release()
}

func TestServerHandlerInterface(_ *testing.T) {
	s := New(Config{})
	s.GET("/ping", func(c *Context) error {
		return c.String(200, "pong")
	})

	adapter := &routerAdapter{server: s}
	var _ stream.Handler = adapter // compile-time check
}

func TestServerCustomNotFound(t *testing.T) {
	s := New(Config{})
	s.GET("/exists", func(c *Context) error {
		return c.String(200, "ok")
	})
	s.NotFound(func(c *Context) error {
		return c.JSON(404, map[string]string{"error": "custom not found"})
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/missing")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 404 {
		t.Fatalf("expected 404, got %d", rw.status)
	}
	if !contains(rw.body, "custom not found") {
		t.Fatalf("expected custom response, got %s", string(rw.body))
	}
	st.Release()
}

func TestServerMethodNotAllowed(t *testing.T) {
	s := New(Config{})
	s.GET("/resource", func(c *Context) error {
		return c.String(200, "get")
	})
	s.POST("/resource", func(c *Context) error {
		return c.String(200, "post")
	})

	adapter := &routerAdapter{server: s}

	// DELETE /resource should return 405.
	st, rw := newTestStream("DELETE", "/resource")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 405 {
		t.Fatalf("expected 405, got %d", rw.status)
	}

	// Verify Allow header is present.
	var allowHeader string
	for _, h := range rw.headers {
		if h[0] == "allow" {
			allowHeader = h[1]
			break
		}
	}
	if allowHeader == "" {
		t.Fatal("expected Allow header")
	}
	if !containsStr(allowHeader, "GET") || !containsStr(allowHeader, "POST") {
		t.Fatalf("expected Allow to contain GET and POST, got %s", allowHeader)
	}
	st.Release()
}

func TestServerCustomMethodNotAllowed(t *testing.T) {
	s := New(Config{})
	s.GET("/resource", func(c *Context) error {
		return c.String(200, "ok")
	})
	s.MethodNotAllowed(func(c *Context) error {
		return c.JSON(405, map[string]string{"error": "custom method not allowed"})
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("POST", "/resource")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 405 {
		t.Fatalf("expected 405, got %d", rw.status)
	}
	if !contains(rw.body, "custom method not allowed") {
		t.Fatalf("expected custom response, got %s", string(rw.body))
	}
	st.Release()
}

func TestServerDoubleStart(t *testing.T) {
	s := New(Config{Addr: ":9091"})
	s.GET("/ping", func(c *Context) error {
		return c.String(200, "pong")
	})

	// Simulate engine already set by firing the startOnce.
	s.startOnce.Do(func() {
		s.engine = &fakeEngine{}
	})

	err := s.Start()
	if err == nil {
		t.Fatal("expected error on double start")
	}
	if !containsStr(err.Error(), "already started") {
		t.Fatalf("expected 'already started' error, got %v", err)
	}
}

func TestServerPanicRecovery(t *testing.T) {
	s := New(Config{})
	s.GET("/panic", func(_ *Context) error {
		panic("handler panic")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/panic")
	// Should not panic — caught by safety net.
	err := adapter.HandleStream(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if rw.status != 500 {
		t.Fatalf("expected 500, got %d", rw.status)
	}
	st.Release()
}

func TestServerStatusCodeAfterHandler(t *testing.T) {
	s := New(Config{})
	var capturedStatus int
	s.Use(func(c *Context) error {
		_ = c.Next()
		capturedStatus = c.StatusCode()
		return nil
	})
	s.GET("/test", func(c *Context) error {
		return c.String(201, "created")
	})

	adapter := &routerAdapter{server: s}

	st, _ := newTestStream("GET", "/test")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if capturedStatus != 201 {
		t.Fatalf("expected middleware to see status 201, got %d", capturedStatus)
	}
	st.Release()
}

// helpers

func contains(b []byte, sub string) bool {
	return containsStr(string(b), sub)
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && searchStr(s, sub)
}

func searchStr(s, sub string) bool {
	for i := range len(s) - len(sub) + 1 {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestServerRouteReturnsRoute(t *testing.T) {
	s := New(Config{})
	route := s.GET("/users/:id", func(c *Context) error {
		return c.String(200, "ok")
	})
	if route == nil {
		t.Fatal("expected non-nil Route")
	}
	// Name should be chainable and return the same route.
	named := route.Name("user-by-id")
	if named != route {
		t.Fatal("expected Name to return same Route")
	}
}

func TestGroupRouteReturnsRoute(t *testing.T) {
	s := New(Config{})
	api := s.Group("/api")
	route := api.GET("/items", func(c *Context) error {
		return c.String(200, "items")
	})
	if route == nil {
		t.Fatal("expected non-nil Route from group")
	}
}

func TestServerHTTPErrorResponse(t *testing.T) {
	s := New(Config{})
	s.GET("/err", func(_ *Context) error {
		return NewHTTPError(422, "validation failed")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/err")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 422 {
		t.Fatalf("expected 422, got %d", rw.status)
	}
	if string(rw.body) != "validation failed" {
		t.Fatalf("expected 'validation failed', got '%s'", string(rw.body))
	}
	st.Release()
}

func TestServerBareErrorResponse(t *testing.T) {
	s := New(Config{})
	s.GET("/err", func(_ *Context) error {
		return fmt.Errorf("something broke")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/err")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 500 {
		t.Fatalf("expected 500, got %d", rw.status)
	}
	if string(rw.body) != "Internal Server Error" {
		t.Fatalf("expected 'Internal Server Error', got '%s'", string(rw.body))
	}
	st.Release()
}

func TestServerMiddlewareErrorHandling(t *testing.T) {
	s := New(Config{})
	s.Use(func(c *Context) error {
		err := c.Next()
		if err != nil {
			// Middleware handles the error — returns nil to prevent safety net.
			return c.JSON(400, map[string]string{"error": err.Error()})
		}
		return nil
	})
	s.GET("/fail", func(_ *Context) error {
		return NewHTTPError(400, "bad request")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/fail")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 400 {
		t.Fatalf("expected 400, got %d", rw.status)
	}
	if !contains(rw.body, "bad request") {
		t.Fatalf("expected error message in body, got '%s'", string(rw.body))
	}
	st.Release()
}

// fakeEngine implements engine.Engine for testing double-start guard.
type fakeEngine struct{}

func (e *fakeEngine) Listen(_ context.Context) error   { return nil }
func (e *fakeEngine) Shutdown(_ context.Context) error { return nil }
func (e *fakeEngine) Metrics() engine.EngineMetrics    { return engine.EngineMetrics{} }
func (e *fakeEngine) Type() engine.EngineType          { return engine.Std }
func (e *fakeEngine) Addr() net.Addr                   { return nil }

func TestServerAny(t *testing.T) {
	s := New(Config{})
	s.Any("/any", func(c *Context) error {
		return c.String(200, "%s", c.Method())
	})

	adapter := &routerAdapter{server: s}

	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		st, rw := newTestStream(method, "/any")
		if err := adapter.HandleStream(context.Background(), st); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if rw.status != 200 {
			t.Fatalf("%s: expected 200, got %d", method, rw.status)
		}
		// HEAD responses may not have body; skip body check for HEAD.
		if method != "HEAD" && string(rw.body) != method {
			t.Fatalf("%s: expected body %q, got %q", method, method, string(rw.body))
		}
		st.Release()
	}
}

func TestServerHandle(t *testing.T) {
	s := New(Config{})
	s.Handle("CUSTOM", "/custom", func(c *Context) error {
		return c.String(200, "custom-method")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("CUSTOM", "/custom")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "custom-method" {
		t.Fatalf("expected 200 custom-method, got %d %s", rw.status, string(rw.body))
	}
	st.Release()
}

func TestGroupMiddleware(t *testing.T) {
	s := New(Config{})

	var groupMWCalled bool
	api := s.Group("/api")
	api.Use(func(c *Context) error {
		groupMWCalled = true
		return c.Next()
	})
	api.GET("/data", func(c *Context) error {
		return c.String(200, "data")
	})

	s.GET("/public", func(c *Context) error {
		return c.String(200, "public")
	})

	adapter := &routerAdapter{server: s}

	// Request to group route should trigger group middleware.
	groupMWCalled = false
	st, rw := newTestStream("GET", "/api/data")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "data" {
		t.Fatalf("expected 200 data, got %d %s", rw.status, string(rw.body))
	}
	if !groupMWCalled {
		t.Fatal("expected group middleware to be called for /api/data")
	}
	st.Release()

	// Request to non-group route should NOT trigger group middleware.
	groupMWCalled = false
	st2, rw2 := newTestStream("GET", "/public")
	if err := adapter.HandleStream(context.Background(), st2); err != nil {
		t.Fatal(err)
	}
	if rw2.status != 200 || string(rw2.body) != "public" {
		t.Fatalf("expected 200 public, got %d %s", rw2.status, string(rw2.body))
	}
	if groupMWCalled {
		t.Fatal("group middleware should not be called for /public")
	}
	st2.Release()
}

func TestGroupHandle(t *testing.T) {
	s := New(Config{})
	api := s.Group("/api")
	api.Handle("PATCH", "/update", func(c *Context) error {
		return c.String(200, "patched")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("PATCH", "/api/update")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "patched" {
		t.Fatalf("expected 200 patched, got %d %s", rw.status, string(rw.body))
	}
	st.Release()
}

func TestServerMiddlewareAppliesToGroup(t *testing.T) {
	s := New(Config{})

	var serverMWCalled bool
	s.Use(func(c *Context) error {
		serverMWCalled = true
		return c.Next()
	})

	api := s.Group("/api")
	api.GET("/data", func(c *Context) error {
		return c.String(200, "ok")
	})

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/api/data")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if !serverMWCalled {
		t.Fatal("server-level middleware must run for group routes")
	}
	if rw.status != 200 || string(rw.body) != "ok" {
		t.Fatalf("expected 200 ok, got %d %s", rw.status, string(rw.body))
	}
	st.Release()
}

func TestServerMiddlewareOrderWithGroup(t *testing.T) {
	s := New(Config{})

	var order []string
	s.Use(func(c *Context) error {
		order = append(order, "server")
		return c.Next()
	})

	api := s.Group("/api", func(c *Context) error {
		order = append(order, "group")
		return c.Next()
	})
	api.GET("/x", func(c *Context) error {
		order = append(order, "handler")
		return c.String(200, "ok")
	})

	adapter := &routerAdapter{server: s}

	st, _ := newTestStream("GET", "/api/x")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "server" || order[1] != "group" || order[2] != "handler" {
		t.Fatalf("expected [server group handler], got %v", order)
	}
	st.Release()
}

func TestServerRoutes(t *testing.T) {
	s := New(Config{})
	s.GET("/", func(_ *Context) error { return nil })
	s.GET("/users/:id", func(_ *Context) error { return nil })
	s.POST("/users", func(_ *Context) error { return nil })
	s.GET("/files/*path", func(_ *Context) error { return nil })

	routes := s.Routes()

	// Build a lookup key for assertions.
	type key struct{ method, path string }
	found := make(map[key]int)
	for _, r := range routes {
		found[key{r.Method, r.Path}] = r.HandlerCount
	}

	expected := []key{
		{"GET", "/"},
		{"GET", "/users/:id"},
		{"POST", "/users"},
		{"GET", "/files/*path"},
	}
	for _, e := range expected {
		if _, ok := found[e]; !ok {
			t.Fatalf("missing route %s %s", e.method, e.path)
		}
	}
	if len(routes) != len(expected) {
		t.Fatalf("expected %d routes, got %d", len(expected), len(routes))
	}
}

func TestServerRoutesHandlerCount(t *testing.T) {
	s := New(Config{})
	mw := func(c *Context) error { return c.Next() }
	s.Use(mw)
	s.GET("/test", func(_ *Context) error { return nil })

	routes := s.Routes()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	// Chain = server middleware + handler = 2.
	if routes[0].HandlerCount != 2 {
		t.Fatalf("expected HandlerCount 2, got %d", routes[0].HandlerCount)
	}
}

func TestServerNamedRouteURL(t *testing.T) {
	s := New(Config{})
	s.GET("/users/:id", func(_ *Context) error { return nil }).Name("user")

	u, err := s.URL("user", "42")
	if err != nil {
		t.Fatal(err)
	}
	if u != "/users/42" {
		t.Fatalf("expected /users/42, got %s", u)
	}
}

func TestServerURLMultipleParams(t *testing.T) {
	s := New(Config{})
	s.GET("/users/:id/posts/:pid", func(_ *Context) error { return nil }).Name("user-post")

	u, err := s.URL("user-post", "7", "99")
	if err != nil {
		t.Fatal(err)
	}
	if u != "/users/7/posts/99" {
		t.Fatalf("expected /users/7/posts/99, got %s", u)
	}
}

func TestServerURLCatchAll(t *testing.T) {
	s := New(Config{})
	s.GET("/files/*filepath", func(_ *Context) error { return nil }).Name("files")

	// Without leading slash.
	u, err := s.URL("files", "css/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if u != "/files/css/style.css" {
		t.Fatalf("expected /files/css/style.css, got %s", u)
	}

	// With leading slash (de-duplicated).
	s2 := New(Config{})
	s2.GET("/files/*filepath", func(_ *Context) error { return nil }).Name("files")
	u2, err := s2.URL("files", "/css/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if u2 != "/files/css/style.css" {
		t.Fatalf("expected /files/css/style.css, got %s", u2)
	}
}

func TestServerURLUnknownName(t *testing.T) {
	s := New(Config{})
	_, err := s.URL("nonexistent")
	if !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("expected ErrRouteNotFound, got %v", err)
	}
}

func TestServerURLWrongParamCount(t *testing.T) {
	s := New(Config{})
	s.GET("/users/:id", func(_ *Context) error { return nil }).Name("user")

	// Too few.
	_, err := s.URL("user")
	if err == nil {
		t.Fatal("expected error for too few params")
	}

	// Too many.
	_, err = s.URL("user", "42", "extra")
	if err == nil {
		t.Fatal("expected error for too many params")
	}
}

func TestServerRoutesAfterSplit(t *testing.T) {
	s := New(Config{})
	s.GET("/api/users", func(_ *Context) error { return nil })
	s.GET("/api/posts", func(_ *Context) error { return nil })

	routes := s.Routes()
	type key struct{ method, path string }
	found := make(map[key]bool)
	for _, r := range routes {
		found[key{r.Method, r.Path}] = true
	}

	if !found[key{"GET", "/api/users"}] {
		t.Fatal("missing GET /api/users")
	}
	if !found[key{"GET", "/api/posts"}] {
		t.Fatal("missing GET /api/posts")
	}
}

func TestRouterSplitPreservesFullPath(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/api/users", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/api/posts", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params
	handlers, fp := r.find("GET", "/api/users", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /api/users")
	}
	if fp != "/api/users" {
		t.Fatalf("expected fullPath /api/users, got %q", fp)
	}

	params = params[:0]
	handlers, fp = r.find("GET", "/api/posts", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /api/posts")
	}
	if fp != "/api/posts" {
		t.Fatalf("expected fullPath /api/posts, got %q", fp)
	}
}

func TestValidatePathEmptyParam(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty param name")
		}
	}()
	s := New(Config{})
	s.GET("/:/:test", func(_ *Context) error { return nil })
}

func TestValidatePathCatchAllNotLast(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for non-terminal catchAll")
		}
	}()
	s := New(Config{})
	s.GET("/files/*path/extra", func(_ *Context) error { return nil })
}

func TestURLCatchAllEmpty(t *testing.T) {
	s := New(Config{})
	s.GET("/files/*filepath", func(_ *Context) error { return nil }).Name("files")
	url, err := s.URL("files", "")
	if err != nil {
		t.Fatal(err)
	}
	if url != "/files" {
		t.Fatalf("expected /files, got %q", url)
	}
}

func TestURLMap(t *testing.T) {
	s := New(Config{})
	s.GET("/users/:id/posts/:pid", func(_ *Context) error { return nil }).Name("user-post")
	s.GET("/files/*filepath", func(_ *Context) error { return nil }).Name("files")

	// Named param substitution.
	url, err := s.URLMap("user-post", map[string]string{"id": "7", "pid": "99"})
	if err != nil {
		t.Fatal(err)
	}
	if url != "/users/7/posts/99" {
		t.Fatalf("expected /users/7/posts/99, got %s", url)
	}

	// CatchAll.
	url, err = s.URLMap("files", map[string]string{"filepath": "/css/style.css"})
	if err != nil {
		t.Fatal(err)
	}
	if url != "/files/css/style.css" {
		t.Fatalf("expected /files/css/style.css, got %s", url)
	}

	// Missing param.
	_, err = s.URLMap("user-post", map[string]string{"id": "7"})
	if err == nil {
		t.Fatal("expected error for missing param")
	}

	// Route not found.
	_, err = s.URLMap("nope", nil)
	if err != ErrRouteNotFound {
		t.Fatalf("expected ErrRouteNotFound, got %v", err)
	}
}

func TestDuplicateRouteName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate route name")
		}
	}()
	s := New(Config{})
	s.GET("/a", func(_ *Context) error { return nil }).Name("dup")
	s.GET("/b", func(_ *Context) error { return nil }).Name("dup")
}

func TestDoubleSlashNormalization(t *testing.T) {
	s := New(Config{})
	h := func(c *Context) error { return c.String(200, "ok") }
	s.GET("/", h)
	s.GET("/api/data", h)

	var params Params
	// "//" should match "/"
	handlers, _ := s.router.find("GET", "//", &params)
	if handlers == nil {
		t.Fatal("expected // to match /")
	}
	// "///api///data" should match "/api/data"
	params = params[:0]
	handlers, fp := s.router.find("GET", "///api///data", &params)
	if handlers == nil {
		t.Fatalf("expected ///api///data to match /api/data")
	}
	if fp != "/api/data" {
		t.Fatalf("expected fullPath /api/data, got %q", fp)
	}
}

func TestServerAddr(t *testing.T) {
	s := New(Config{})
	if s.Addr() != nil {
		t.Fatal("expected nil addr before start")
	}
}

func TestTryNameSuccess(t *testing.T) {
	s := New(Config{})
	route := s.GET("/users/:id", func(_ *Context) error { return nil })
	if err := route.TryName("user"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	u, err := s.URL("user", "42")
	if err != nil {
		t.Fatal(err)
	}
	if u != "/users/42" {
		t.Fatalf("expected /users/42, got %s", u)
	}
}

func TestTryNameDuplicate(t *testing.T) {
	s := New(Config{})
	r1 := s.GET("/a", func(_ *Context) error { return nil })
	r2 := s.GET("/b", func(_ *Context) error { return nil })

	if err := r1.TryName("dup"); err != nil {
		t.Fatalf("first TryName should succeed: %v", err)
	}
	err := r2.TryName("dup")
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !errors.Is(err, ErrDuplicateRouteName) {
		t.Fatalf("expected ErrDuplicateRouteName, got %v", err)
	}
}

// Phase 1.2 tests

func TestConfigSurfacePassthrough(t *testing.T) {
	cfg := Config{
		Addr:                 ":9090",
		MaxConcurrentStreams: 200,
		MaxFrameSize:         32768,
		InitialWindowSize:    131072,
		MaxHeaderBytes:       1 << 20,
		DisableKeepAlive:     true,
		BufferSize:           65536,
		SocketRecvBuf:        262144,
		SocketSendBuf:        262144,
		MaxConns:             1000,
		Workers:              4,
	}

	rc := cfg.toResourceConfig()

	if rc.MaxConcurrentStreams != 200 {
		t.Fatalf("MaxConcurrentStreams: got %d", rc.MaxConcurrentStreams)
	}
	if rc.MaxFrameSize != 32768 {
		t.Fatalf("MaxFrameSize: got %d", rc.MaxFrameSize)
	}
	if rc.InitialWindowSize != 131072 {
		t.Fatalf("InitialWindowSize: got %d", rc.InitialWindowSize)
	}
	if rc.MaxHeaderBytes != 1<<20 {
		t.Fatalf("MaxHeaderBytes: got %d", rc.MaxHeaderBytes)
	}
	if !rc.DisableKeepAlive {
		t.Fatal("DisableKeepAlive not mapped")
	}
	if rc.Resources.BufferSize != 65536 {
		t.Fatalf("BufferSize: got %d", rc.Resources.BufferSize)
	}
	if rc.Resources.SocketRecv != 262144 {
		t.Fatalf("SocketRecv: got %d", rc.Resources.SocketRecv)
	}
	if rc.Resources.SocketSend != 262144 {
		t.Fatalf("SocketSend: got %d", rc.Resources.SocketSend)
	}
	if rc.Resources.MaxConns != 1000 {
		t.Fatalf("MaxConns: got %d", rc.Resources.MaxConns)
	}
	if rc.Resources.Workers != 4 {
		t.Fatalf("Workers: got %d", rc.Resources.Workers)
	}
}

func TestConfigZeroValuesNotMapped(t *testing.T) {
	cfg := Config{Addr: ":8080"}
	rc := cfg.toResourceConfig()

	if rc.Resources.BufferSize != 0 {
		t.Fatalf("expected 0 BufferSize, got %d", rc.Resources.BufferSize)
	}
	if rc.Resources.SocketRecv != 0 {
		t.Fatalf("expected 0 SocketRecv, got %d", rc.Resources.SocketRecv)
	}
	if rc.Resources.SocketSend != 0 {
		t.Fatalf("expected 0 SocketSend, got %d", rc.Resources.SocketSend)
	}
	if rc.Resources.MaxConns != 0 {
		t.Fatalf("expected 0 MaxConns, got %d", rc.Resources.MaxConns)
	}
}

// Phase 2.1 tests

func TestRouteUseMiddleware(t *testing.T) {
	s := New(Config{})
	var routeMWCalled bool

	s.GET("/a", func(c *Context) error {
		return c.String(200, "a")
	}).Use(func(c *Context) error {
		routeMWCalled = true
		return c.Next()
	})

	s.GET("/b", func(c *Context) error {
		return c.String(200, "b")
	})

	adapter := &routerAdapter{server: s}

	// Route with middleware.
	routeMWCalled = false
	st, rw := newTestStream("GET", "/a")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "a" {
		t.Fatalf("expected 200 a, got %d %s", rw.status, string(rw.body))
	}
	if !routeMWCalled {
		t.Fatal("expected route middleware to be called")
	}
	st.Release()

	// Route without middleware — should not trigger route-specific middleware.
	routeMWCalled = false
	st2, rw2 := newTestStream("GET", "/b")
	if err := adapter.HandleStream(context.Background(), st2); err != nil {
		t.Fatal(err)
	}
	if rw2.status != 200 || string(rw2.body) != "b" {
		t.Fatalf("expected 200 b, got %d %s", rw2.status, string(rw2.body))
	}
	if routeMWCalled {
		t.Fatal("route middleware should not affect other routes")
	}
	st2.Release()
}

func TestRouteUseChainOrder(t *testing.T) {
	s := New(Config{})
	var order []string

	s.Use(func(c *Context) error {
		order = append(order, "server")
		return c.Next()
	})

	s.GET("/test", func(c *Context) error {
		order = append(order, "handler")
		return c.String(200, "ok")
	}).Use(func(c *Context) error {
		order = append(order, "route")
		return c.Next()
	})

	adapter := &routerAdapter{server: s}
	st, _ := newTestStream("GET", "/test")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "server" || order[1] != "route" || order[2] != "handler" {
		t.Fatalf("expected [server route handler], got %v", order)
	}
	st.Release()
}

// Phase 5.1 tests

func TestInheritListenerNotSet(t *testing.T) {
	ln, err := InheritListener("CELERIS_TEST_NONEXISTENT_ENV_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if ln != nil {
		t.Fatal("expected nil listener when env var not set")
	}
}

func TestStartWithListenerDoubleStart(t *testing.T) {
	s := New(Config{})
	// Simulate engine already set by firing the startOnce.
	s.startOnce.Do(func() {
		s.engine = &fakeEngine{}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	err = s.StartWithListener(ln)
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("expected ErrAlreadyStarted, got %v", err)
	}
}

func TestServerDisableMetrics(t *testing.T) {
	s := New(Config{DisableMetrics: true})
	s.GET("/ping", func(c *Context) error {
		return c.String(200, "pong")
	})

	// Simulate prepare without actually starting.
	s.startOnce.Do(func() {
		s.engine = &fakeEngine{}
	})
	// When DisableMetrics is true, collector should remain nil.
	if s.Collector() != nil {
		t.Fatal("expected nil Collector when DisableMetrics is true")
	}
}

func TestRoutesOutputOrder(t *testing.T) {
	s := New(Config{})
	s.GET("/b", func(_ *Context) error { return nil })
	s.POST("/a", func(_ *Context) error { return nil })
	s.DELETE("/c", func(_ *Context) error { return nil })
	s.GET("/a", func(_ *Context) error { return nil })

	routes := s.Routes()
	if len(routes) != 4 {
		t.Fatalf("expected 4 routes, got %d", len(routes))
	}

	// Verify deterministic order: sorted by method first.
	// DELETE < GET < POST
	if routes[0].Method != "DELETE" {
		t.Fatalf("expected first method DELETE, got %s", routes[0].Method)
	}
	if routes[1].Method != "GET" {
		t.Fatalf("expected second method GET, got %s", routes[1].Method)
	}

	// Call again and verify same order.
	routes2 := s.Routes()
	for i := range routes {
		if routes[i].Method != routes2[i].Method || routes[i].Path != routes2[i].Path {
			t.Fatalf("Routes() output not deterministic at index %d", i)
		}
	}
}

func TestAdaptHandler(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Adapted", "true")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("adapted"))
	})
	adapted := Adapt(h)
	if adapted == nil {
		t.Fatal("expected non-nil handler from Adapt")
	}

	s := New(Config{})
	s.GET("/adapt", adapted)
	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/adapt")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 201 {
		t.Fatalf("expected 201, got %d", rw.status)
	}
	if string(rw.body) != "adapted" {
		t.Fatalf("expected body 'adapted', got %q", string(rw.body))
	}
	var found bool
	for _, h := range rw.headers {
		if h[0] == "x-adapted" && h[1] == "true" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected x-adapted header")
	}
	st.Release()
}

func TestAdaptFuncHandler(t *testing.T) {
	fn := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from " + r.Method))
	}
	adapted := AdaptFunc(fn)
	if adapted == nil {
		t.Fatal("expected non-nil handler from AdaptFunc")
	}

	s := New(Config{})
	s.POST("/func", adapted)
	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("POST", "/func")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "hello from POST" {
		t.Fatalf("expected 'hello from POST', got %q", string(rw.body))
	}
	st.Release()
}

func TestPrepareFailedThenRetry(t *testing.T) {
	// Use an invalid addr to trigger config validation failure.
	s := New(Config{Addr: "not-a-valid-addr"})
	s.GET("/ping", func(c *Context) error {
		return c.String(200, "pong")
	})

	// First call should fail with config validation error.
	_, err := s.prepare()
	if err == nil {
		t.Fatal("expected error from invalid config")
	}
	if !containsStr(err.Error(), "config validation") {
		t.Fatalf("expected config validation error, got %v", err)
	}

	// Second call should return the same config error (not ErrAlreadyStarted),
	// because sync.Once remembers the first execution's outcome.
	_, err2 := s.prepare()
	if err2 == nil {
		t.Fatal("expected error on retry")
	}
	if err2.Error() != err.Error() {
		t.Fatalf("expected same error on retry, got %v (original: %v)", err2, err)
	}
}

func TestStaticFileServing(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(dir+"/style.css", []byte("body{}"), 0644)

	s := New(Config{})
	s.Static("/assets", dir)

	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/assets/style.css")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "body{}" {
		t.Fatalf("expected body{}, got %s", string(rw.body))
	}
	st.Release()

	// Directory traversal should fail.
	st2, rw2 := newTestStream("GET", "/assets/../../../etc/passwd")
	if err := adapter.HandleStream(context.Background(), st2); err != nil {
		t.Fatal(err)
	}
	if rw2.status != 400 {
		t.Fatalf("expected 400 for traversal, got %d", rw2.status)
	}
	st2.Release()
}

func TestStartRaceGuard(t *testing.T) {
	s := New(Config{Addr: ":0"})
	s.GET("/ping", func(c *Context) error {
		return c.String(200, "pong")
	})

	// Fire the once to prevent actual engine creation.
	s.startOnce.Do(func() {
		s.engine = &fakeEngine{}
	})

	// Multiple concurrent calls should all get ErrAlreadyStarted, no race.
	errs := make(chan error, 10)
	for range 10 {
		go func() {
			_, err := s.prepare()
			errs <- err
		}()
	}
	for range 10 {
		err := <-errs
		if !errors.Is(err, ErrAlreadyStarted) {
			t.Fatalf("expected ErrAlreadyStarted, got %v", err)
		}
	}
}
