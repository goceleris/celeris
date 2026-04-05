package proxy

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// Tests use multi-entry XFF chains so the middleware's right-to-left walk
// (which sets SetClientIP) produces a different result than Context.ClientIP's
// native leftmost-entry fallback. This lets us distinguish "middleware set the
// override" from "Context read the header natively".

func TestEmptyTrustedProxiesNoOp(t *testing.T) {
	mw := New(Config{})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("1.2.3.4:1234"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (no override), got %q", ip)
	}
}

func TestUntrustedPeerIgnoresHeaders(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("203.0.113.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (untrusted peer), got %q", ip)
	}
}

func TestTrustedPeerXFFSingleIP(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected ClientIP=5.6.7.8, got %q", ip)
	}
}

func TestTrustedPeerXFFChainRightToLeft(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8", "192.168.0.0/16"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "1.1.1.1, 2.2.2.2, 192.168.1.1, 10.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "2.2.2.2" {
		t.Fatalf("expected ClientIP=2.2.2.2, got %q", ip)
	}
}

func TestTrustedPeerXFFAllTrusted(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "10.0.0.2, 10.0.0.3"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "10.0.0.2" {
		t.Fatalf("expected native ClientIP=10.0.0.2, got %q", ip)
	}
}

func TestTrustedPeerXRealIP(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-real-ip", "9.8.7.6"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "9.8.7.6" {
		t.Fatalf("expected ClientIP=9.8.7.6, got %q", ip)
	}
}

func TestXFFPrecedenceOverXRealIP(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
		celeristest.WithHeader("x-real-ip", "9.8.7.6"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected XFF to take precedence: ClientIP=5.6.7.8, got %q", ip)
	}
}

func TestMalformedXFFStopsWalk(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, not-an-ip, 10.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (malformed XFF), got %q", ip)
	}
}

func TestIPv4MappedIPv6InXFF(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8, ::ffff:10.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected ClientIP=5.6.7.8 (mapped IPv6 trusted), got %q", ip)
	}
}

func TestForwardedProtoHTTPS(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-proto", "https"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if s := ctx.Scheme(); s != "https" {
		t.Fatalf("expected scheme=https, got %q", s)
	}
}

func TestForwardedProtoInvalidIgnored(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-proto", "ftp"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if s := ctx.Scheme(); s != "ftp" && s != "http" {
		t.Fatalf("expected scheme=ftp (native) or http, got %q", s)
	}
}

func TestForwardedHost(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-host", "example.com"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if h := ctx.Host(); h != "example.com" {
		t.Fatalf("expected host=example.com, got %q", h)
	}
}

func TestSkipPathsBypass(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		SkipPaths:      []string{"/health"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/health",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (skip path), got %q", ip)
	}
}

func TestSkipFuncBypass(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/bypass"
		},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/bypass",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (skip func), got %q", ip)
	}
}

func TestParseTrustedProxiesPanicOnInvalid(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on invalid CIDR/IP")
		}
	}()
	New(Config{
		TrustedProxies: []string{"not-valid"},
	})
}

func TestBareIPTrustedProxy(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.1"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected ClientIP=5.6.7.8 with bare IP proxy, got %q", ip)
	}
}

func TestXRealIPOnlyConfig(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"X-Real-Ip"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
		celeristest.WithHeader("x-real-ip", "9.8.7.6"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "9.8.7.6" {
		t.Fatalf("expected ClientIP=9.8.7.6 (X-Real-Ip only), got %q", ip)
	}
}

func TestForwardedProtoDisabled(t *testing.T) {
	mw := New(Config{
		TrustedProxies:        []string{"10.0.0.0/8"},
		DisableForwardedProto: true,
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-proto", "https"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	// DisableForwardedProto=true -> middleware does NOT call SetScheme.
}

func TestForwardedHostDisabled(t *testing.T) {
	mw := New(Config{
		TrustedProxies:       []string{"10.0.0.0/8"},
		DisableForwardedHost: true,
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-host", "example.com"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if h := ctx.Host(); h != "localhost" {
		t.Fatalf("expected native host=localhost (DisableForwardedHost=true), got %q", h)
	}
}

// --- Fix 7: new test cases ---

func TestPureIPv6PeerTrusted(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"::1/128"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("[::1]:80"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected ClientIP=5.6.7.8 (IPv6 peer trusted), got %q", ip)
	}
}

func TestIPv4MappedIPv6PeerTrusted(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("[::ffff:10.0.0.1]:80"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected ClientIP=5.6.7.8 (IPv4-mapped IPv6 peer trusted), got %q", ip)
	}
}

func TestEmptyRemoteAddrNoOp(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr(""),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	// Empty RemoteAddr -> extractPeerIP returns false -> no-op.
}

func TestXRealIPInvalidValueIgnored(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"x-real-ip"},
	})
	// Provide a multi-entry XFF so native ClientIP returns the leftmost ("100.0.0.1").
	// XFF is not in TrustedHeaders, so the middleware ignores it.
	// X-Real-IP is invalid -> middleware does not call SetClientIP.
	// If the middleware had called SetClientIP("not-a-valid-ip"), ClientIP()
	// would return that. Instead, native fallback reads XFF leftmost.
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-real-ip", "not-a-valid-ip"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	// Native ClientIP reads XFF leftmost ("100.0.0.1") since no override was set.
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (invalid X-Real-IP ignored), got %q", ip)
	}
}

func TestXForwardedHostDangerousCharsRejected(t *testing.T) {
	dangerous := []string{
		"evil.com\r\nInjected: true",
		"evil.com\nInjected: true",
		"evil.com/path",
		"evil.com\\path",
		"evil.com?q=1",
		"evil.com#frag",
		"user@evil.com",
		"evil.com\x00null",
	}
	for _, host := range dangerous {
		mw := New(Config{
			TrustedProxies: []string{"10.0.0.0/8"},
		})
		ctx, _ := celeristest.NewContextT(t, "GET", "/",
			celeristest.WithRemoteAddr("10.0.0.1:1234"),
			celeristest.WithHeader("x-forwarded-host", host),
		)
		if err := mw(ctx); err != nil {
			t.Fatal(err)
		}
		if h := ctx.Host(); h != "localhost" {
			t.Fatalf("dangerous host %q should be rejected, but Host()=%q", host, h)
		}
	}
}

func TestCustomHeaderCFConnectingIP(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"cf-connecting-ip"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("cf-connecting-ip", "203.0.113.50"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "203.0.113.50" {
		t.Fatalf("expected ClientIP=203.0.113.50 from cf-connecting-ip, got %q", ip)
	}
}

func TestCustomHeaderInvalidValueIgnored(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"cf-connecting-ip"},
	})
	// Provide XFF so native ClientIP returns leftmost ("100.0.0.1").
	// cf-connecting-ip is invalid -> middleware does not call SetClientIP.
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("cf-connecting-ip", "not-an-ip"),
		celeristest.WithHeader("x-forwarded-for", "100.0.0.1, 200.0.0.2"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	// Native ClientIP reads XFF leftmost since no override was set.
	if ip := ctx.ClientIP(); ip != "100.0.0.1" {
		t.Fatalf("expected native ClientIP=100.0.0.1 (invalid custom header), got %q", ip)
	}
}

func TestCustomHeaderXFFPrecedence(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"x-forwarded-for", "cf-connecting-ip"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
		celeristest.WithHeader("cf-connecting-ip", "203.0.113.50"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	// XFF is checked first, so 5.6.7.8 wins.
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected XFF to take precedence over custom header, got %q", ip)
	}
}

func TestZeroConfigEnabledByDefault(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-for", "5.6.7.8"),
		celeristest.WithHeader("x-forwarded-proto", "https"),
		celeristest.WithHeader("x-forwarded-host", "example.com"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if ip := ctx.ClientIP(); ip != "5.6.7.8" {
		t.Fatalf("expected ClientIP=5.6.7.8, got %q", ip)
	}
	if s := ctx.Scheme(); s != "https" {
		t.Fatalf("expected scheme=https, got %q", s)
	}
	if h := ctx.Host(); h != "example.com" {
		t.Fatalf("expected host=example.com, got %q", h)
	}
}

func TestZeroConfigDisableFieldsAreFalse(t *testing.T) {
	cfg := Config{}
	if cfg.DisableForwardedProto {
		t.Fatal("zero-value Config should have DisableForwardedProto=false (enabled)")
	}
	if cfg.DisableForwardedHost {
		t.Fatal("zero-value Config should have DisableForwardedHost=false (enabled)")
	}
}

func TestZeroConfigForwardedProtoHostEnabled(t *testing.T) {
	cfg := Config{TrustedProxies: []string{"10.0.0.0/8"}}
	// Go zero value: DisableForwardedProto and DisableForwardedHost are false,
	// meaning both features are enabled by default (safe zero value).
	if cfg.DisableForwardedProto {
		t.Fatal("zero-value Config should have DisableForwardedProto=false (enabled)")
	}
	if cfg.DisableForwardedHost {
		t.Fatal("zero-value Config should have DisableForwardedHost=false (enabled)")
	}
}

func TestXRealIPIPv4MappedNormalized(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"x-real-ip"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-real-ip", "::ffff:9.8.7.6"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	// ::ffff:9.8.7.6 should be unmapped to 9.8.7.6.
	if ip := ctx.ClientIP(); ip != "9.8.7.6" {
		t.Fatalf("expected ClientIP=9.8.7.6 (unmapped), got %q", ip)
	}
}

func TestWalkXFFNormalizesIP(t *testing.T) {
	// walkXFF should return normalized IP strings (e.g. leading zeros trimmed).
	nets := parseTrustedProxies([]string{"10.0.0.0/8"})
	result := walkXFF("5.6.7.8, 10.0.0.2", nets)
	if result != "5.6.7.8" {
		t.Fatalf("expected walkXFF to return 5.6.7.8, got %q", result)
	}
}

func TestIsValidHost(t *testing.T) {
	valid := []string{
		"example.com",
		"example.com:8080",
		"sub.example.com",
		"192.168.1.1",
		"[::1]",
	}
	for _, h := range valid {
		if !isValidHost(h) {
			t.Fatalf("expected %q to be valid", h)
		}
	}

	invalid := []string{
		"",
		"evil\r\n",
		"evil\n",
		"evil\x00",
		"evil/path",
		"evil\\path",
		"evil?q=1",
		"evil#frag",
		"user@evil",
	}
	for _, h := range invalid {
		if isValidHost(h) {
			t.Fatalf("expected %q to be invalid", h)
		}
	}
}

func TestIsValidHostRejectsOversized(t *testing.T) {
	long := strings.Repeat("a", 254)
	if isValidHost(long) {
		t.Fatal("expected host exceeding 253 bytes to be invalid")
	}
	exact := strings.Repeat("a", 253)
	if !isValidHost(exact) {
		t.Fatal("expected 253-byte host to be valid")
	}
}

func TestValidateEmptyTrustedHeaderPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on empty TrustedHeaders entry")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "must not contain empty strings") {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()
	New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"x-forwarded-for", ""},
	})
}

func TestValidateWhitespaceTrustedHeaderPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on whitespace-only TrustedHeaders entry")
		}
	}()
	New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustedHeaders: []string{"  "},
	})
}

func TestWalkXFFEmptySegments(t *testing.T) {
	nets := parseTrustedProxies([]string{"10.0.0.0/8"})
	result := walkXFF("1.1.1.1, , 2.2.2.2", nets)
	if result != "2.2.2.2" {
		t.Fatalf("expected walkXFF to return 2.2.2.2 (skip empty segments), got %q", result)
	}
}

func TestWalkXFFAllEmptySegments(t *testing.T) {
	nets := parseTrustedProxies([]string{"10.0.0.0/8"})
	result := walkXFF(", , ", nets)
	if result != "" {
		t.Fatalf("expected walkXFF to return empty on all-empty segments, got %q", result)
	}
}

func TestXForwardedHostBackslashRejected(t *testing.T) {
	mw := New(Config{
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	ctx, _ := celeristest.NewContextT(t, "GET", "/",
		celeristest.WithRemoteAddr("10.0.0.1:1234"),
		celeristest.WithHeader("x-forwarded-host", "evil.com\\path"),
	)
	if err := mw(ctx); err != nil {
		t.Fatal(err)
	}
	if h := ctx.Host(); h != "localhost" {
		t.Fatalf("backslash host should be rejected, but Host()=%q", h)
	}
}
