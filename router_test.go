package celeris

import (
	"testing"
)

func TestRouterStaticRoutes(t *testing.T) {
	r := newRouter()
	called := ""
	r.addRoute("GET", "/", []HandlerFunc{func(_ *Context) error { called = "/"; return nil }})
	r.addRoute("GET", "/hello", []HandlerFunc{func(_ *Context) error { called = "/hello"; return nil }})
	r.addRoute("GET", "/hello/world", []HandlerFunc{func(_ *Context) error { called = "/hello/world"; return nil }})

	var params Params

	handlers, fp := r.find("GET", "/", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /")
	}
	_ = handlers[0](nil)
	if called != "/" {
		t.Fatalf("expected /, got %s", called)
	}
	if fp != "/" {
		t.Fatalf("expected fullPath /, got %s", fp)
	}

	params = params[:0]
	handlers, fp = r.find("GET", "/hello", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /hello")
	}
	_ = handlers[0](nil)
	if called != "/hello" {
		t.Fatalf("expected /hello, got %s", called)
	}
	if fp != "/hello" {
		t.Fatalf("expected fullPath /hello, got %s", fp)
	}

	params = params[:0]
	handlers, fp = r.find("GET", "/hello/world", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /hello/world")
	}
	_ = handlers[0](nil)
	if called != "/hello/world" {
		t.Fatalf("expected /hello/world, got %s", called)
	}
	if fp != "/hello/world" {
		t.Fatalf("expected fullPath /hello/world, got %s", fp)
	}
}

func TestRouterParamRoutes(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/users/:id", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/users/:id/posts/:pid", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params

	handlers, fp := r.find("GET", "/users/42", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /users/42")
	}
	if len(params) != 1 || params[0].Key != "id" || params[0].Value != "42" {
		t.Fatalf("expected id=42, got %+v", params)
	}
	if fp != "/users/:id" {
		t.Fatalf("expected fullPath /users/:id, got %s", fp)
	}

	params = params[:0]
	handlers, fp = r.find("GET", "/users/7/posts/99", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /users/7/posts/99")
	}
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d: %+v", len(params), params)
	}
	if params[0].Key != "id" || params[0].Value != "7" {
		t.Fatalf("expected id=7, got %+v", params[0])
	}
	if params[1].Key != "pid" || params[1].Value != "99" {
		t.Fatalf("expected pid=99, got %+v", params[1])
	}
	if fp != "/users/:id/posts/:pid" {
		t.Fatalf("expected fullPath /users/:id/posts/:pid, got %s", fp)
	}
}

func TestRouterCatchAll(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/files/*filepath", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params
	handlers, fp := r.find("GET", "/files/css/style.css", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /files/css/style.css")
	}
	if len(params) != 1 || params[0].Key != "filepath" {
		t.Fatalf("expected filepath param, got %+v", params)
	}
	if fp != "/files/*filepath" {
		t.Fatalf("expected fullPath /files/*filepath, got %s", fp)
	}
}

func TestRouterNotFound(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/hello", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params
	handlers, _ := r.find("GET", "/notfound", &params)
	if handlers != nil {
		t.Fatal("expected nil handlers for /notfound")
	}

	handlers, _ = r.find("POST", "/hello", &params)
	if handlers != nil {
		t.Fatal("expected nil handlers for POST /hello")
	}
}

func TestRouterMethodSeparation(t *testing.T) {
	r := newRouter()
	getCalled := false
	postCalled := false
	r.addRoute("GET", "/test", []HandlerFunc{func(_ *Context) error { getCalled = true; return nil }})
	r.addRoute("POST", "/test", []HandlerFunc{func(_ *Context) error { postCalled = true; return nil }})

	var params Params
	handlers, _ := r.find("GET", "/test", &params)
	if handlers == nil {
		t.Fatal("expected GET handler")
	}
	_ = handlers[0](nil)
	if !getCalled {
		t.Fatal("GET handler not called")
	}

	handlers, _ = r.find("POST", "/test", &params)
	if handlers == nil {
		t.Fatal("expected POST handler")
	}
	_ = handlers[0](nil)
	if !postCalled {
		t.Fatal("POST handler not called")
	}
}

func TestRouterAllowedMethods(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/resource", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("POST", "/resource", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("PUT", "/resource", []HandlerFunc{func(_ *Context) error { return nil }})

	allowed := r.allowedMethods("/resource", "DELETE")
	if len(allowed) != 3 {
		t.Fatalf("expected 3 allowed methods, got %d: %v", len(allowed), allowed)
	}

	// Exclude GET — should return POST, PUT.
	allowed = r.allowedMethods("/resource", "GET")
	if len(allowed) != 2 {
		t.Fatalf("expected 2 allowed methods, got %d: %v", len(allowed), allowed)
	}

	// Unknown path — no methods.
	allowed = r.allowedMethods("/missing", "GET")
	if len(allowed) != 0 {
		t.Fatalf("expected 0 allowed methods, got %d: %v", len(allowed), allowed)
	}
}

func TestRouterOverlappingPaths(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/api/users", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/api/users/:id", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/api/posts", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params

	handlers, _ := r.find("GET", "/api/users", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /api/users")
	}

	params = params[:0]
	handlers, _ = r.find("GET", "/api/users/123", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /api/users/123")
	}
	if len(params) != 1 || params[0].Value != "123" {
		t.Fatalf("expected id=123, got %+v", params)
	}

	params = params[:0]
	handlers, _ = r.find("GET", "/api/posts", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /api/posts")
	}
}

func TestRouterFullPath(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/users/:id", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/files/*path", []HandlerFunc{func(_ *Context) error { return nil }})
	r.addRoute("GET", "/static/about", []HandlerFunc{func(_ *Context) error { return nil }})

	tests := []struct {
		path    string
		wantFP  string
		wantNil bool
	}{
		{"/", "/", false},
		{"/users/42", "/users/:id", false},
		{"/files/a/b/c", "/files/*path", false},
		{"/static/about", "/static/about", false},
		{"/missing", "", true},
	}

	for _, tt := range tests {
		var params Params
		handlers, fp := r.find("GET", tt.path, &params)
		if tt.wantNil {
			if handlers != nil {
				t.Fatalf("path %s: expected nil handlers", tt.path)
			}
			continue
		}
		if handlers == nil {
			t.Fatalf("path %s: expected handlers", tt.path)
		}
		if fp != tt.wantFP {
			t.Fatalf("path %s: expected fullPath %q, got %q", tt.path, tt.wantFP, fp)
		}
	}
}

func TestRouteName(t *testing.T) {
	r := newRouter()
	route := r.addRoute("GET", "/users/:id", []HandlerFunc{func(_ *Context) error { return nil }})
	named := route.Name("user-by-id")
	if named != route {
		t.Fatal("expected Name to return same Route")
	}
	if route.name != "user-by-id" {
		t.Fatalf("expected name 'user-by-id', got %q", route.name)
	}
}

func TestRouterStaticPriorityOverParam(t *testing.T) {
	r := newRouter()
	paramCalled := false
	staticCalled := false
	// Register param BEFORE static — static must still win.
	r.addRoute("GET", "/users/:id", []HandlerFunc{func(_ *Context) error { paramCalled = true; return nil }})
	r.addRoute("GET", "/users/new", []HandlerFunc{func(_ *Context) error { staticCalled = true; return nil }})

	var params Params

	// /users/new should match the static route.
	handlers, _ := r.find("GET", "/users/new", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /users/new")
	}
	_ = handlers[0](nil)
	if !staticCalled {
		t.Fatal("expected static handler to be called for /users/new")
	}
	if paramCalled {
		t.Fatal("param handler should not be called for /users/new")
	}

	// /users/42 should still match the param route.
	paramCalled = false
	staticCalled = false
	params = params[:0]
	handlers, _ = r.find("GET", "/users/42", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /users/42")
	}
	_ = handlers[0](nil)
	if !paramCalled {
		t.Fatal("expected param handler to be called for /users/42")
	}
	if len(params) != 1 || params[0].Value != "42" {
		t.Fatalf("expected id=42, got %+v", params)
	}
}

func TestCatchAllDedup(t *testing.T) {
	r := newRouter()
	h1 := []HandlerFunc{func(_ *Context) error { return nil }}
	h2 := []HandlerFunc{func(_ *Context) error { return nil }}

	r.addRoute("GET", "/files/*filepath", h1)
	r.addRoute("GET", "/files/*path", h2)

	var params Params
	handlers, _ := r.find("GET", "/files/a.txt", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /files/a.txt")
	}
	// Both registrations should share the same catchall node.
	// The first key ("filepath") wins because it created the node.
	if len(params) != 1 || params[0].Key != "filepath" {
		t.Fatalf("expected param key 'filepath', got %+v", params)
	}
}

func TestTrailingSlashSeparateRoutes(t *testing.T) {
	r := newRouter()
	withSlash := false
	withoutSlash := false
	r.addRoute("GET", "/users", []HandlerFunc{func(_ *Context) error { withoutSlash = true; return nil }})
	r.addRoute("GET", "/users/", []HandlerFunc{func(_ *Context) error { withSlash = true; return nil }})

	var params Params

	handlers, _ := r.find("GET", "/users", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /users")
	}
	_ = handlers[0](nil)
	if !withoutSlash {
		t.Fatal("expected /users handler")
	}

	handlers, _ = r.find("GET", "/users/", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /users/")
	}
	_ = handlers[0](nil)
	if !withSlash {
		t.Fatal("expected /users/ handler")
	}
}

func TestParamAtRoot(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/:id", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params
	handlers, fp := r.find("GET", "/42", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /42")
	}
	if len(params) != 1 || params[0].Key != "id" || params[0].Value != "42" {
		t.Fatalf("expected id=42, got %+v", params)
	}
	if fp != "/:id" {
		t.Fatalf("expected fullPath /:id, got %s", fp)
	}
}

func TestCatchAllAtRoot(t *testing.T) {
	r := newRouter()
	r.addRoute("GET", "/*filepath", []HandlerFunc{func(_ *Context) error { return nil }})

	var params Params
	handlers, fp := r.find("GET", "/a/b/c", &params)
	if handlers == nil {
		t.Fatal("expected handlers for /a/b/c")
	}
	if len(params) != 1 || params[0].Key != "filepath" {
		t.Fatalf("expected filepath param, got %+v", params)
	}
	if fp != "/*filepath" {
		t.Fatalf("expected fullPath /*filepath, got %s", fp)
	}
}

func TestParamsGet(t *testing.T) {
	params := Params{
		{Key: "id", Value: "42"},
		{Key: "name", Value: "alice"},
	}

	// Found case.
	v, ok := params.Get("id")
	if !ok || v != "42" {
		t.Fatalf("expected (42, true), got (%s, %v)", v, ok)
	}

	v, ok = params.Get("name")
	if !ok || v != "alice" {
		t.Fatalf("expected (alice, true), got (%s, %v)", v, ok)
	}

	// Not-found case.
	v, ok = params.Get("missing")
	if ok || v != "" {
		t.Fatalf("expected ('', false), got (%s, %v)", v, ok)
	}
}
