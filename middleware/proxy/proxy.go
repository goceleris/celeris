package proxy

import (
	"net/netip"
	"strings"

	"github.com/goceleris/celeris"
)

// New creates a proxy middleware that extracts real client IP, scheme, and host
// from trusted reverse proxy headers. Pass zero or one Config; zero-value
// fields are filled with safe defaults.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	trustedNets := parseTrustedProxies(cfg.TrustedProxies)

	headers := make([]string, len(cfg.TrustedHeaders))
	for i, h := range cfg.TrustedHeaders {
		headers[i] = strings.ToLower(h)
	}

	hasXFF := false
	hasXRI := false
	var otherHeaders []string
	for _, h := range headers {
		switch h {
		case "x-forwarded-for":
			hasXFF = true
		case "x-real-ip":
			hasXRI = true
		default:
			otherHeaders = append(otherHeaders, h)
		}
	}

	forwardedProto := cfg.ForwardedProto
	forwardedHost := cfg.ForwardedHost

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		if len(trustedNets) == 0 {
			return c.Next()
		}

		peer, ok := extractPeerIP(c.RemoteAddr())
		if !ok {
			return c.Next()
		}

		if !isTrusted(peer, trustedNets) {
			return c.Next()
		}

		var clientIP string
		if hasXFF {
			if xff := c.Header("x-forwarded-for"); xff != "" {
				clientIP = walkXFF(xff, trustedNets)
			}
		}
		if clientIP == "" && hasXRI {
			if xri := c.Header("x-real-ip"); xri != "" {
				raw := strings.TrimSpace(xri)
				if addr, err := netip.ParseAddr(raw); err == nil {
					clientIP = addr.Unmap().String()
				}
			}
		}
		if clientIP == "" {
			for _, h := range otherHeaders {
				if val := c.Header(h); val != "" {
					raw := strings.TrimSpace(val)
					if addr, err := netip.ParseAddr(raw); err == nil {
						clientIP = addr.Unmap().String()
						break
					}
				}
			}
		}

		if clientIP != "" {
			c.SetClientIP(clientIP)
		}

		if forwardedProto {
			if proto := c.Header("x-forwarded-proto"); proto != "" {
				p := strings.ToLower(strings.TrimSpace(proto))
				if p == "http" || p == "https" {
					c.SetScheme(p)
				}
			}
		}

		if forwardedHost {
			if host := c.Header("x-forwarded-host"); host != "" {
				h := strings.TrimSpace(host)
				if isValidHost(h) {
					c.SetHost(h)
				}
			}
		}

		return c.Next()
	}
}

// extractPeerIP extracts the IP from a "host:port" address.
// Handles IPv6 bracket notation. Normalizes IPv4-mapped IPv6 via Unmap.
func extractPeerIP(addr string) (netip.Addr, bool) {
	if addr == "" {
		return netip.Addr{}, false
	}
	host, _, err := splitHostPort(addr)
	if err != nil {
		host = addr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// splitHostPort is a lightweight host:port splitter that avoids net.SplitHostPort's
// allocation. It handles bracketed IPv6 ([::1]:port).
func splitHostPort(addr string) (string, string, error) {
	if len(addr) == 0 {
		return "", "", errMissingPort
	}
	if addr[0] == '[' {
		end := strings.IndexByte(addr, ']')
		if end < 0 {
			return "", "", errMissingBracket
		}
		host := addr[1:end]
		if end+1 < len(addr) && addr[end+1] == ':' {
			return host, addr[end+2:], nil
		}
		return host, "", nil
	}
	colon := strings.LastIndexByte(addr, ':')
	if colon < 0 {
		return addr, "", errMissingPort
	}
	return addr[:colon], addr[colon+1:], nil
}

var (
	errMissingPort    = &splitError{"missing port"}
	errMissingBracket = &splitError{"missing ]"}
)

type splitError struct{ s string }

func (e *splitError) Error() string { return e.s }

// isTrusted checks if addr falls within any of the trusted prefixes.
func isTrusted(addr netip.Addr, nets []netip.Prefix) bool {
	for _, n := range nets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// walkXFF walks X-Forwarded-For right-to-left, skipping trusted IPs.
// Returns the first untrusted IP (normalized), or empty string if all are
// trusted or a malformed entry is encountered.
// Uses LastIndexByte to avoid allocating a []string slice.
func walkXFF(xff string, nets []netip.Prefix) string {
	for {
		var entry string
		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			entry = strings.TrimSpace(xff[i+1:])
			xff = xff[:i]
		} else {
			entry = strings.TrimSpace(xff)
			xff = ""
		}
		if entry == "" {
			if xff == "" {
				break
			}
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return ""
		}
		addr = addr.Unmap()
		if !isTrusted(addr, nets) {
			return addr.String()
		}
		if xff == "" {
			break
		}
	}
	return ""
}

// maxHostLength is the DNS hostname maximum per RFC 1035.
const maxHostLength = 253

// isValidHost returns true if the host string does not contain
// characters that could enable header injection or path traversal.
func isValidHost(host string) bool {
	if host == "" || len(host) > maxHostLength {
		return false
	}
	for i := 0; i < len(host); i++ {
		switch host[i] {
		case '\r', '\n', '\x00', '/', '\\', '?', '#', '@':
			return false
		}
	}
	return true
}
