package overload_test

import (
	"github.com/goceleris/celeris/middleware/overload"
	"github.com/goceleris/celeris/observe"
)

// ExampleConfig — the middleware needs a [Config.CollectorProvider]
// to sample CPU/queue/latency signals. A celeris.Server's
// [observe.Collector] is the typical source; any function returning a
// `*observe.Collector` works.
//
// In production wire it as `app.Use(overload.New(cfg))` after the
// server is constructed and its collector is reachable.
func ExampleConfig() {
	_ = overload.Config{
		CollectorProvider: func() *observe.Collector { return nil },
	}
}
