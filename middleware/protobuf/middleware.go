package protobuf

import (
	"github.com/goceleris/celeris"
	"google.golang.org/protobuf/proto"
)

const configKey = "protobuf.config"

// New creates a middleware that stores protobuf [Config] in the request
// context for later retrieval via [FromContext]. This enables route handlers
// to use custom marshal/unmarshal options without passing them explicitly.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}

	return func(c *celeris.Context) error {
		c.Set(configKey, &cfg)
		return c.Next()
	}
}

// Helper provides protobuf operations using the [Config] stored by the
// [New] middleware.
type Helper struct {
	c   *celeris.Context
	cfg *Config
}

// FromContext returns a [Helper] using the config stored by the [New]
// middleware. If the middleware was not installed, returns a Helper with
// default options.
func FromContext(c *celeris.Context) *Helper {
	cfg := &defaultConfig
	if v, ok := c.Get(configKey); ok {
		if stored, ok := v.(*Config); ok {
			cfg = stored
		}
	}
	return &Helper{c: c, cfg: cfg}
}

// Write serializes v as protobuf using the stored [Config.MarshalOptions].
func (h *Helper) Write(code int, v proto.Message) error {
	return protoBufWith(h.c, code, v, h.cfg.MarshalOptions)
}

// Bind reads the request body and unmarshals it using the stored
// [Config.UnmarshalOptions]. Returns [ErrNotProtoBuf] if the content
// type is not protobuf.
func (h *Helper) Bind(v proto.Message) error {
	ct := h.c.Header("content-type")
	if !isProtoBufContentType(ct) {
		return ErrNotProtoBuf
	}
	return bindProtoBufWith(h.c, v, h.cfg.UnmarshalOptions)
}
