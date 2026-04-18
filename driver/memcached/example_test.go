package memcached_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/goceleris/celeris/driver/memcached"
)

// ExampleNewClient shows the basic Set/Get loop. The client is lazy: the
// first command triggers the TCP dial.
func ExampleNewClient() {
	client, err := memcached.NewClient("localhost:11211")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Set(ctx, "greeting", "hello, world", time.Minute); err != nil {
		log.Fatal(err)
	}
	v, err := client.Get(ctx, "greeting")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v)
}

// ExampleClient_Get illustrates handling the cache-miss sentinel.
func ExampleClient_Get() {
	client, _ := memcached.NewClient("localhost:11211")
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	v, err := client.Get(ctx, "session:42")
	switch {
	case errors.Is(err, memcached.ErrCacheMiss):
		fmt.Println("miss")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Println(v)
	}
}

// ExampleClient_Set stores a value with a 10-minute TTL.
func ExampleClient_Set() {
	client, _ := memcached.NewClient("localhost:11211")
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	if err := client.Set(ctx, "counter", "0", 10*time.Minute); err != nil {
		log.Fatal(err)
	}
}

// ExampleClient_GetMulti fetches several keys in one round trip (text
// protocol) or sequential GETs (binary protocol).
func ExampleClient_GetMulti() {
	client, _ := memcached.NewClient("localhost:11211")
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	out, err := client.GetMulti(ctx, "user:1", "user:2", "user:3")
	if err != nil {
		log.Fatal(err)
	}
	for k, v := range out {
		fmt.Println(k, "=", v)
	}
}

// ExampleClient_CAS performs an optimistic read-modify-write.
func ExampleClient_CAS() {
	client, _ := memcached.NewClient("localhost:11211")
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	item, err := client.Gets(ctx, "counter")
	if err != nil {
		log.Fatal(err)
	}
	newValue := string(item.Value) + "!"
	ok, err := client.CAS(ctx, "counter", newValue, item.CAS, time.Hour)
	if err != nil && !errors.Is(err, memcached.ErrCASConflict) {
		log.Fatal(err)
	}
	fmt.Println("stored:", ok)
}
