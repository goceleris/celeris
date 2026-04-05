package proxy_test

import (
	"github.com/goceleris/celeris/middleware/proxy"
)

func ExampleNew() {
	// Trust Cloudflare and local reverse proxy.
	_ = proxy.New(proxy.Config{
		TrustedProxies: []string{"173.245.48.0/20", "10.0.0.0/8"},
	})
}

func ExampleNew_realIPOnly() {
	// Only use X-Real-Ip (ignore X-Forwarded-For).
	_ = proxy.New(proxy.Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"X-Real-Ip"},
	})
}

func ExampleDefaultConfig() {
	// Use DefaultConfig for a pre-populated config with ForwardedProto
	// and ForwardedHost enabled.
	cfg := proxy.DefaultConfig()
	cfg.TrustedProxies = []string{"10.0.0.0/8"}
	_ = proxy.New(cfg)
}

func ExampleNew_customHeader() {
	// Trust Cloudflare's CF-Connecting-IP header.
	_ = proxy.New(proxy.Config{
		TrustedProxies: []string{"173.245.48.0/20"},
		TrustedHeaders: []string{"cf-connecting-ip"},
	})
}
