package proxy

import (
	"net/netip"
	"strings"

	"github.com/goceleris/celeris"
)

// Config controls proxy header extraction behavior.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// TrustedProxies lists CIDRs or bare IPs whose forwarded headers are trusted.
	// Bare IPs are expanded to /32 (IPv4) or /128 (IPv6).
	// An empty list means no headers are trusted (safe default -- middleware is a no-op).
	// Invalid entries cause a panic at init time.
	TrustedProxies []string

	// TrustedHeaders lists which forwarded headers to inspect.
	// "x-forwarded-for" and "x-real-ip" have built-in chain-walk and validation
	// logic. Any other header name (e.g. "cf-connecting-ip") is treated as a
	// single-value IP header: the value is parsed with netip.ParseAddr and used
	// only if valid.
	// Values are lowercased at init time.
	// Default: ["x-forwarded-for", "x-real-ip"].
	TrustedHeaders []string

	// DisableForwardedProto disables processing of X-Forwarded-Proto.
	// When false (default), X-Forwarded-Proto is used to override Scheme.
	DisableForwardedProto bool

	// DisableForwardedHost disables processing of X-Forwarded-Host.
	// When false (default), X-Forwarded-Host is used to override Host.
	DisableForwardedHost bool
}

var defaultConfig = Config{
	TrustedHeaders: []string{"x-forwarded-for", "x-real-ip"},
}

func applyDefaults(cfg Config) Config {
	if len(cfg.TrustedHeaders) == 0 {
		cfg.TrustedHeaders = defaultConfig.TrustedHeaders
	}
	return cfg
}

// validate checks Config invariants. parseTrustedProxies already panics on
// invalid CIDR/IP entries; this method covers remaining constraints.
func (cfg Config) validate() {
	for _, h := range cfg.TrustedHeaders {
		if strings.TrimSpace(h) == "" {
			panic("proxy: TrustedHeaders must not contain empty strings")
		}
	}
}

// parseTrustedProxies parses CIDRs and bare IPs into []netip.Prefix.
// Panics on invalid entries.
func parseTrustedProxies(proxies []string) []netip.Prefix {
	nets := make([]netip.Prefix, 0, len(proxies))
	for _, entry := range proxies {
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			nets = append(nets, prefix)
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			panic("proxy: invalid trusted proxy entry: " + entry)
		}
		bits := 128
		if addr.Is4() {
			bits = 32
		}
		nets = append(nets, netip.PrefixFrom(addr, bits))
	}
	return nets
}
