// Package proxy extracts the real client IP, scheme, and host from trusted
// reverse proxy headers.
//
// When a celeris server sits behind a load balancer or reverse proxy
// (e.g. Nginx, Cloudflare, AWS ALB), the TCP peer is the proxy, not the end
// user. [New] returns a middleware that inspects X-Forwarded-For, X-Real-Ip,
// X-Forwarded-Proto, and X-Forwarded-Host -- but only when the immediate peer
// is in the configured [Config.TrustedProxies] list. An empty TrustedProxies
// list is the safe default: the middleware becomes a no-op and trusts nothing,
// preventing IP spoofing. There is no "trust all" toggle; specify CIDRs (or
// "0.0.0.0/0") explicitly. Invalid entries panic at [New].
//
// Run it via [celeris.Server.Pre] so the overrides take effect before routing
// and before any middleware reads ClientIP, Scheme, or Host:
//
//	s.Pre(proxy.New(proxy.Config{
//	    TrustedProxies: []string{"10.0.0.0/8", "172.16.0.0/12"},
//	}))
//
// [Config.TrustedHeaders] selects which headers to inspect (default
// "x-forwarded-for" and "x-real-ip"); X-Forwarded-For is walked right-to-left
// to defeat left-side spoofing, while any other listed header is treated as a
// single-value IP. The Disable* fields use the zero-value-enabled convention:
// [Config.DisableForwardedProto] and [Config.DisableForwardedHost] both default
// to false, so a minimal Config processes both headers. Use [Config.SkipPaths]
// or [Config.Skip] to bypass the middleware. RFC 7239 (Forwarded) is not
// implemented.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-security
package proxy
