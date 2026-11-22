package compress

import (
	"encoding/json"
	"math/rand/v2"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// jsonItems builds a deterministic JSON list of objects shaped like the
// HttpArena json-comp dataset (id, name, category, price, quantity, active,
// tags, rating), cut at the first item that takes the body to size bytes. A
// realistic body for the cost of an encoder per response, where the repeated
// sentence of benchBody is not: the field names repeat, the values do not.
func jsonItems(size int) []byte {
	adj := []string{"Alpha", "Core", "Edge", "Flex", "Hyper", "Nano", "Prime", "Pro", "Swift", "Ultra"}
	noun := []string{"Driver", "Gadget", "Gear", "Link", "Module", "Node", "Relay", "Switch", "Valve", "Widget"}
	cat := []string{"electronics", "tools", "books", "clothing", "sports", "toys", "home", "garden", "office", "auto"}
	tag := []string{"compact", "eco", "fast", "heavy-duty", "limited", "new", "popular", "premium", "sale", "wireless"}
	type rating struct {
		Score int `json:"score"`
		Count int `json:"count"`
	}
	type item struct {
		ID       int      `json:"id"`
		Name     string   `json:"name"`
		Category string   `json:"category"`
		Price    int      `json:"price"`
		Quantity int      `json:"quantity"`
		Active   bool     `json:"active"`
		Tags     []string `json:"tags"`
		Rating   rating   `json:"rating"`
	}
	r := rand.New(rand.NewPCG(1, 2))
	var items []item
	for id := 1; ; id++ {
		it := item{
			ID:       id,
			Name:     adj[r.IntN(len(adj))] + " " + noun[r.IntN(len(noun))],
			Category: cat[r.IntN(len(cat))],
			Price:    50 + r.IntN(450),
			Quantity: 1 + r.IntN(100),
			Active:   r.IntN(5) != 0,
			Rating:   rating{Score: 10 + r.IntN(40), Count: 1 + r.IntN(500)},
		}
		for n := 2 + r.IntN(3); n > 0; n-- {
			it.Tags = append(it.Tags, tag[r.IntN(len(tag))])
		}
		items = append(items, it)
		b, err := json.Marshal(map[string]any{"items": items, "count": len(items)})
		if err != nil {
			panic(err)
		}
		if len(b) >= size {
			return b
		}
	}
}

// BenchmarkEncodingCost is the cost of one compressed response, through the
// middleware at the default level of each encoder, for a JSON body of about
// 1, 10 and 100 KB: ns/op and allocs/op of the whole round (context, handler,
// negotiation, encoding, flush), bytes_in/op and bytes_out/op of the body.
// "identity" is the same round with no Accept-Encoding, the floor the three
// encoders add to. Run with -cpu 1 to count the GC the allocs cause in ns/op.
func BenchmarkEncodingCost(b *testing.B) {
	for _, sz := range []struct {
		name string
		size int
	}{{"1KB", 1 << 10}, {"10KB", 10 << 10}, {"100KB", 100 << 10}} {
		body := jsonItems(sz.size)
		handler := func(c *celeris.Context) error {
			return c.Blob(200, "application/json", body)
		}
		for _, enc := range []string{"identity", "gzip", "br", "zstd"} {
			b.Run(sz.name+"/"+enc, func(b *testing.B) {
				mw := New(Config{Encodings: []string{"gzip", "br", "zstd"}})
				opts := []celeristest.Option{celeristest.WithHandlers(mw, handler)}
				if enc != "identity" {
					opts = append(opts, celeristest.WithHeader("accept-encoding", enc))
				}
				ctx, rec := celeristest.NewContext("GET", "/data", opts...)
				_ = ctx.Next()
				out := len(rec.Body)
				if got := rec.Header("content-encoding"); enc != "identity" && got != enc {
					b.Fatalf("Content-Encoding %q, want %q", got, enc)
				}
				celeristest.ReleaseContext(ctx)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					ctx, _ := celeristest.NewContext("GET", "/data", opts...)
					_ = ctx.Next()
					celeristest.ReleaseContext(ctx)
				}
				b.ReportMetric(float64(len(body)), "bytes_in/op")
				b.ReportMetric(float64(out), "bytes_out/op")
			})
		}
	}
}
