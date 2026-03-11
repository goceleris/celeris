package celeris_test

import (
	"fmt"

	"github.com/goceleris/celeris"
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
