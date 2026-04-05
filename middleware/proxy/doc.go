// Package proxy extracts real client IP, scheme, and host from trusted
// reverse proxy headers.
//
// When a celeris server sits behind a load balancer or reverse proxy
// (e.g. Nginx, Cloudflare, AWS ALB), the TCP peer address is the proxy,
// not the end user. This middleware inspects X-Forwarded-For, X-Real-Ip,
// X-Forwarded-Proto, and X-Forwarded-Host headers -- but only when the
// immediate peer is in the configured [Config].TrustedProxies list.
//
// # Security Model
//
// An empty TrustedProxies list is the safe default: the middleware becomes
// a no-op and never trusts any forwarded header. This prevents IP spoofing
// when the server is exposed directly to the internet.
//
// There is no TrustAllProxies option. Trusting all proxies defeats the
// purpose of the right-to-left walk and opens the door to trivial
// IP spoofing. If you need to trust all proxies in a controlled
// environment, specify the CIDR explicitly (e.g. "0.0.0.0/0").
//
// # DefaultConfig
//
// Use [DefaultConfig] to get a pre-populated [Config] with ForwardedProto
// and ForwardedHost enabled, then override specific fields:
//
//	cfg := proxy.DefaultConfig()
//	cfg.TrustedProxies = []string{"10.0.0.0/8"}
//	server.Pre(proxy.New(cfg))
//
// When passing a [Config] literal directly, ForwardedProto and
// ForwardedHost default to false (Go zero value). Set them explicitly
// if you need them.
//
// # TrustedProxies
//
// Accepts CIDR notation ("10.0.0.0/8") and bare IPs ("10.0.0.1").
// Bare IPs are expanded to /32 (IPv4) or /128 (IPv6) at init time.
// Invalid entries cause a panic so misconfigurations surface immediately.
//
// Common provider CIDR ranges:
//   - Cloudflare: https://www.cloudflare.com/ips/
//   - AWS CloudFront: https://ip-ranges.amazonaws.com/ip-ranges.json
//   - GCP: https://cloud.google.com/load-balancing/docs/https#target-proxies
//
// # TrustedHeaders
//
// By default, "x-forwarded-for" and "x-real-ip" are inspected. These two
// headers have built-in logic: XFF is walked right-to-left, and X-Real-IP
// is validated with net/netip.ParseAddr.
//
// Any other header name added to TrustedHeaders (e.g. "cf-connecting-ip",
// "true-client-ip") is treated as a single-value IP header. The value is
// parsed with netip.ParseAddr and used only if valid. Custom headers are
// checked after XFF and X-Real-IP, in order of appearance.
//
// # X-Forwarded-For Walk
//
// The middleware walks the X-Forwarded-For chain right-to-left, skipping
// entries that match a trusted network. The first untrusted IP is taken as
// the real client IP. This algorithm is resilient against left-side
// spoofing because attacker-controlled entries appear on the left, while
// each trusted proxy appends to the right.
//
// If a malformed (non-IP) entry is encountered during the walk, traversal
// stops and no client IP override is applied, falling back to the peer
// address.
//
// # X-Forwarded-Host Validation
//
// When ForwardedHost is enabled, the X-Forwarded-Host value is validated
// before being applied. Values containing \r, \n, \x00, /, \, ?, #, or @
// are rejected to prevent header injection and path traversal. Hosts
// exceeding 253 bytes (the DNS maximum per RFC 1035) are also rejected.
//
// # IPv4-Mapped IPv6
//
// IPv4-mapped IPv6 addresses (::ffff:10.0.0.1) are normalized via
// netip.Addr.Unmap so that a trusted network specified as 10.0.0.0/8
// correctly matches the mapped form.
//
// # ForwardedProto and ForwardedHost
//
// When enabled, X-Forwarded-Proto overrides [celeris.Context.Scheme]
// and X-Forwarded-Host overrides [celeris.Context.Host]. Only "http" and
// "https" are accepted for proto; other values are silently ignored.
//
// # RFC 7239 (Forwarded)
//
// The standard Forwarded header (RFC 7239) is not currently implemented.
// Most real-world proxies emit X-Forwarded-For, and the middleware focuses
// on that de facto standard. RFC 7239 support may be added in a future
// release.
//
// # Runs via Server.Pre
//
// This middleware is designed to run via [celeris.Server.Pre] so that the
// overrides take effect before routing and before any downstream middleware
// reads ClientIP, Scheme, or Host.
//
//	s := celeris.New(celeris.Config{Addr: ":8080"})
//	s.Pre(proxy.New(proxy.Config{
//	    TrustedProxies: []string{"10.0.0.0/8", "172.16.0.0/12"},
//	}))
//
// # Skip
//
// Use [Config].SkipPaths for exact path matches or [Config].Skip for
// dynamic bypass logic.
package proxy
