package redis_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

// ExampleNewClient shows the basic Get/Set loop. The client is lazy: the
// first command triggers the TCP dial and the RESP3 (or RESP2) handshake.
func ExampleNewClient() {
	client, err := redis.NewClient("localhost:6379",
		redis.WithPassword("secret"),
		redis.WithDB(0),
	)
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

// ExampleClient_Pipeline batches several commands into one network write.
// Results are harvested from typed *Cmd handles after Exec returns.
func ExampleClient_Pipeline() {
	client, err := redis.NewClient("localhost:6379")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	p := client.Pipeline()
	setRes := p.Set("counter", 0, 0)
	incrRes := p.Incr("counter")
	ttlRes := p.Expire("counter", time.Hour)

	if err := p.Exec(ctx); err != nil {
		log.Fatal(err)
	}
	_, _ = setRes.Result()
	n, _ := incrRes.Result()
	ok, _ := ttlRes.Result()
	fmt.Println(n, ok)
}

// ExampleClient_Subscribe opens a dedicated pub/sub connection and consumes
// messages from [PubSub.Channel]. A goroutine publishes one message via the
// cmd-pool client to drive the subscriber.
func ExampleClient_Subscribe() {
	client, err := redis.NewClient("localhost:6379")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ps, err := client.Subscribe(ctx, "events")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = ps.Close() }()

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = client.Publish(ctx, "events", "hello")
	}()

	select {
	case m := <-ps.Channel():
		fmt.Println(m.Channel, m.Payload)
	case <-ctx.Done():
		log.Fatal(ctx.Err())
	}
}

// ExampleClient_Do is the escape hatch for commands the typed API does not
// cover. Args are stringified through the driver's internal argify helper;
// the reply is a detached [protocol.Value] safe to retain.
func ExampleClient_Do() {
	client, err := redis.NewClient("localhost:6379")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	// XADD stream * field value — returns the new entry ID as a bulk string.
	id, err := client.DoString(ctx, "XADD", "events", "*", "field", "value")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(id)
}

// ExampleClient_TxPipeline queues a pair of INCRs under a single MULTI/EXEC
// transaction. Per-command results are deferred until Exec returns.
func ExampleClient_TxPipeline() {
	client, err := redis.NewClient("localhost:6379")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	tx, err := client.TxPipeline(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = tx.Discard() }() // no-op after a successful Exec.

	visits := tx.Incr("visits")
	uniques := tx.Incr("uniques")
	if err := tx.Exec(ctx); err != nil {
		log.Fatal(err)
	}
	v, _ := visits.Result()
	u, _ := uniques.Result()
	fmt.Println(v, u)
}
