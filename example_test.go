package celeris_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func Example() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.GET("/hello", func(c *celeris.Context) error {
		return c.String(200, "Hello, World!")
	})
	// s.Start() blocks until shutdown
	_ = s // prevent unused error in example
	fmt.Println("server configured")
	// Output: server configured
}

func ExampleServer_Use() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.Use(func(c *celeris.Context) error {
		fmt.Println("middleware executed")
		return c.Next()
	})
	s.GET("/", func(c *celeris.Context) error {
		return c.String(200, "ok")
	})
	fmt.Println("middleware registered")
	// Output: middleware registered
}

func ExampleNewHTTPError() {
	err := celeris.NewHTTPError(404, "user not found")
	fmt.Println(err.Error())
	// Output: code=404, message=user not found
}

func ExampleServer_Group() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	api := s.Group("/api")
	api.GET("/users", func(_ *celeris.Context) error { return nil })
	api.GET("/posts", func(_ *celeris.Context) error { return nil })
	fmt.Println("group registered")
	// Output: group registered
}

func ExampleServer_Routes() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.GET("/a", func(_ *celeris.Context) error { return nil })
	s.POST("/b", func(_ *celeris.Context) error { return nil })
	routes := s.Routes()
	fmt.Println(len(routes))
	// Output: 2
}

func ExampleServer_URL() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.GET("/users/:id", func(_ *celeris.Context) error { return nil }).Name("user")
	url, err := s.URL("user", "42")
	fmt.Println(url, err)
	// Output: /users/42 <nil>
}

func ExampleServer_NotFound() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.NotFound(func(c *celeris.Context) error {
		return c.JSON(404, map[string]string{"error": "not found"})
	})
	fmt.Println("custom 404 set")
	// Output: custom 404 set
}

func ExampleAdapt() {
	_ = celeris.Adapt(nil) // wraps any net/http Handler
	fmt.Println("adapter created")
	// Output: adapter created
}

func ExampleContext_JSON() {
	ctx, rec := celeristest.NewContext("GET", "/api/user")
	defer celeristest.ReleaseContext(ctx)

	_ = ctx.JSON(200, map[string]string{"name": "alice"})
	fmt.Println(rec.StatusCode)
	fmt.Println(rec.Header("content-type"))
	var m map[string]string
	_ = json.Unmarshal(rec.Body, &m)
	fmt.Println(m["name"])
	// Output:
	// 200
	// application/json
	// alice
}

func ExampleContext_Bind() {
	type User struct {
		Name string `json:"name"`
	}
	body := []byte(`{"name":"bob"}`)
	ctx, _ := celeristest.NewContext("POST", "/users",
		celeristest.WithBody(body),
		celeristest.WithContentType("application/json"),
	)
	defer celeristest.ReleaseContext(ctx)

	var u User
	err := ctx.Bind(&u)
	fmt.Println(err)
	fmt.Println(u.Name)
	// Output:
	// <nil>
	// bob
}

func ExampleContext_BufferResponse() {
	ctx, rec := celeristest.NewContext("GET", "/data")
	defer celeristest.ReleaseContext(ctx)

	ctx.BufferResponse()
	_ = ctx.String(200, "original")

	// Transform the buffered body before sending.
	body := ctx.ResponseBody()
	ctx.SetResponseBody([]byte(strings.ToUpper(string(body))))
	_ = ctx.FlushResponse()

	fmt.Println(rec.StatusCode)
	fmt.Println(rec.BodyString())
	// Output:
	// 200
	// ORIGINAL
}

func ExampleContext_Hijack() {
	ctx, _ := celeristest.NewContext("GET", "/ws")
	defer celeristest.ReleaseContext(ctx)

	// celeristest uses a mock writer that does not implement Hijacker,
	// so Hijack returns ErrHijackNotSupported.
	_, err := ctx.Hijack()
	fmt.Println(err)
	// Output: celeris: hijack not supported by this engine
}

func ExampleContext_StreamWriter() {
	ctx, _ := celeristest.NewContext("GET", "/events")
	defer celeristest.ReleaseContext(ctx)

	// celeristest mock does not implement Streamer, so StreamWriter is nil.
	sw := ctx.StreamWriter()
	fmt.Println(sw == nil)
	// Output: true
}

func ExampleContext_RemoteAddr() {
	// RemoteAddr returns empty from celeristest by default.
	ctx, _ := celeristest.NewContext("GET", "/info")
	defer celeristest.ReleaseContext(ctx)
	fmt.Printf("addr=%q\n", ctx.RemoteAddr())
	// Output: addr=""
}

func ExampleContext_Negotiate() {
	ctx, _ := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept", "application/xml, application/json;q=0.9"),
	)
	defer celeristest.ReleaseContext(ctx)

	best := ctx.Negotiate("application/json", "application/xml")
	fmt.Println(best)
	// Output: application/xml
}

func ExampleContext_FormValue() {
	ctx, _ := celeristest.NewContext("POST", "/submit",
		celeristest.WithBody([]byte("name=alice&color=blue")),
		celeristest.WithContentType("application/x-www-form-urlencoded"),
	)
	defer celeristest.ReleaseContext(ctx)

	fmt.Println(ctx.FormValue("name"))
	fmt.Println(ctx.FormValue("color"))
	// Output:
	// alice
	// blue
}

func ExampleContext_Cookie() {
	ctx, rec := celeristest.NewContext("GET", "/test",
		celeristest.WithCookie("session", "abc123"),
	)
	defer celeristest.ReleaseContext(ctx)

	val, err := ctx.Cookie("session")
	fmt.Println(val, err)

	ctx.SetCookie(&celeris.Cookie{Name: "token", Value: "xyz", HTTPOnly: true})
	_ = ctx.NoContent(200)
	fmt.Println(rec.Header("set-cookie"))
	// Output:
	// abc123 <nil>
	// token=xyz; HttpOnly
}

func ExampleContext_Redirect() {
	ctx, rec := celeristest.NewContext("GET", "/old")
	defer celeristest.ReleaseContext(ctx)

	_ = ctx.Redirect(301, "/new")
	fmt.Println(rec.StatusCode)
	fmt.Println(rec.Header("location"))
	// Output:
	// 301
	// /new
}

func ExampleContext_BasicAuth() {
	ctx, _ := celeristest.NewContext("GET", "/admin",
		celeristest.WithBasicAuth("alice", "secret"),
	)
	defer celeristest.ReleaseContext(ctx)

	user, _, ok := ctx.BasicAuth()
	fmt.Println(ok, user)
	// Output: true alice
}

func ExampleContext_Query() {
	ctx, _ := celeristest.NewContext("GET", "/search",
		celeristest.WithQuery("q", "celeris"),
		celeristest.WithQuery("page", "2"),
	)
	defer celeristest.ReleaseContext(ctx)

	fmt.Println(ctx.Query("q"))
	fmt.Println(ctx.QueryDefault("limit", "10"))
	// Output:
	// celeris
	// 10
}

func ExampleContext_Param() {
	ctx, _ := celeristest.NewContext("GET", "/users/42",
		celeristest.WithParam("id", "42"),
	)
	defer celeristest.ReleaseContext(ctx)

	fmt.Println(ctx.Param("id"))
	// Output: 42
}

func ExampleContext_File() {
	dir, _ := os.MkdirTemp("", "celeris-example-*")
	defer func() { _ = os.RemoveAll(dir) }()
	_ = os.WriteFile(dir+"/hello.txt", []byte("hello"), 0644)

	ctx, rec := celeristest.NewContext("GET", "/download")
	defer celeristest.ReleaseContext(ctx)

	_ = ctx.File(dir + "/hello.txt")
	fmt.Println(rec.StatusCode)
	fmt.Println(rec.BodyString())
	// Output:
	// 200
	// hello
}

func ExampleServer_Static() {
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.Static("/assets", "./public")
	routes := s.Routes()
	fmt.Println(routes[0].Method, routes[0].Path)
	// Output: GET /assets/*filepath
}

func ExampleServer_StartWithContext() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	_ = ctx // prevent unused error
	s := celeris.New(celeris.Config{Addr: ":8080"})
	s.GET("/ping", func(c *celeris.Context) error {
		return c.String(200, "pong")
	})
	// In a real app: log.Fatal(s.StartWithContext(ctx))
	fmt.Println("configured")
	// Output: configured
}

func ExampleContext_Respond() {
	ctx, rec := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept", "application/json"),
	)
	defer celeristest.ReleaseContext(ctx)

	_ = ctx.Respond(200, map[string]string{"key": "value"})
	fmt.Println(rec.Header("content-type"))
	var m map[string]string
	_ = json.Unmarshal(rec.Body, &m)
	fmt.Println(m["key"])
	// Output:
	// application/json
	// value
}

func ExampleContext_XML() {
	type Item struct {
		Name string
	}
	ctx, rec := celeristest.NewContext("GET", "/item")
	defer celeristest.ReleaseContext(ctx)

	_ = ctx.XML(200, Item{Name: "widget"})
	fmt.Println(rec.StatusCode)
	fmt.Println(rec.Header("content-type"))
	// Output:
	// 200
	// application/xml
}

func ExampleContext_Attachment() {
	ctx, rec := celeristest.NewContext("GET", "/download")
	defer celeristest.ReleaseContext(ctx)

	ctx.Attachment("report.pdf")
	_ = ctx.Blob(200, "application/pdf", []byte("content"))
	fmt.Println(rec.Header("content-disposition"))
	// Output: attachment; filename="report.pdf"
}

func ExampleContext_IsWebSocket() {
	ctx, _ := celeristest.NewContext("GET", "/ws",
		celeristest.WithHeader("upgrade", "websocket"),
	)
	defer celeristest.ReleaseContext(ctx)

	fmt.Println(ctx.IsWebSocket())
	// Output: true
}

func ExampleContext_AcceptsEncodings() {
	ctx, _ := celeristest.NewContext("GET", "/data",
		celeristest.WithHeader("accept-encoding", "gzip, br;q=0.8"),
	)
	defer celeristest.ReleaseContext(ctx)

	fmt.Println(ctx.AcceptsEncodings("br", "gzip"))
	// Output: gzip
}
