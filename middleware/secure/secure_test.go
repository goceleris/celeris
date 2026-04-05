package secure

import (
	"reflect"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func TestDefaultConfigSetsAllHeaders(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeader(t, rec, "x-content-type-options", "nosniff")
	testutil.AssertHeader(t, rec, "x-frame-options", "SAMEORIGIN")
	testutil.AssertHeader(t, rec, "x-xss-protection", "0")
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=63072000; includeSubDomains")
	testutil.AssertHeader(t, rec, "referrer-policy", "strict-origin-when-cross-origin")
	testutil.AssertHeader(t, rec, "cross-origin-opener-policy", "same-origin")
	testutil.AssertHeader(t, rec, "cross-origin-resource-policy", "same-origin")
	testutil.AssertHeader(t, rec, "cross-origin-embedder-policy", "require-corp")
	testutil.AssertHeader(t, rec, "x-dns-prefetch-control", "off")
	testutil.AssertHeader(t, rec, "x-permitted-cross-domain-policies", "none")
	testutil.AssertHeader(t, rec, "origin-agent-cluster", "?1")
	testutil.AssertHeader(t, rec, "x-download-options", "noopen")
}

func TestDefaultConfigOmitsCSPAndPermissionsPolicy(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "content-security-policy")
	testutil.AssertNoHeader(t, rec, "content-security-policy-report-only")
	testutil.AssertNoHeader(t, rec, "permissions-policy")
}

func TestHeaderDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *Config
		header string
		want   string
	}{
		{"xss-protection default", nil, "x-xss-protection", "0"},
		{"xss-protection custom", &Config{XSSProtection: "1; mode=block"}, "x-xss-protection", "1; mode=block"},
		{"origin-agent-cluster default", nil, "origin-agent-cluster", "?1"},
		{"origin-agent-cluster custom", &Config{OriginAgentCluster: "?0"}, "origin-agent-cluster", "?0"},
		{"x-download-options default", nil, "x-download-options", "noopen"},
		{"x-download-options custom", &Config{XDownloadOptions: "custom"}, "x-download-options", "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mw celeris.HandlerFunc
			if tt.cfg == nil {
				mw = New()
			} else {
				mw = New(*tt.cfg)
			}
			chain := []celeris.HandlerFunc{mw, okHandler}
			rec, err := testutil.RunChain(t, chain, "GET", "/")
			testutil.AssertNoError(t, err)
			testutil.AssertHeader(t, rec, tt.header, tt.want)
		})
	}
}

func TestCustomValues(t *testing.T) {
	mw := New(Config{
		XContentTypeOptions:       "custom-cto",
		XFrameOptions:             "DENY",
		HSTSMaxAge:                31536000,
		HSTSExcludeSubdomains:     true,
		ReferrerPolicy:            "no-referrer",
		CrossOriginOpenerPolicy:   "unsafe-none",
		CrossOriginResourcePolicy: "cross-origin",
		CrossOriginEmbedderPolicy: "unsafe-none",
		XDNSPrefetchControl:       "on",
		XPermittedCrossDomain:     "master-only",
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-content-type-options", "custom-cto")
	testutil.AssertHeader(t, rec, "x-frame-options", "DENY")
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=31536000")
	testutil.AssertHeader(t, rec, "referrer-policy", "no-referrer")
	testutil.AssertHeader(t, rec, "cross-origin-opener-policy", "unsafe-none")
	testutil.AssertHeader(t, rec, "cross-origin-resource-policy", "cross-origin")
	testutil.AssertHeader(t, rec, "cross-origin-embedder-policy", "unsafe-none")
	testutil.AssertHeader(t, rec, "x-dns-prefetch-control", "on")
	testutil.AssertHeader(t, rec, "x-permitted-cross-domain-policies", "master-only")
}

func TestHSTSIncludeSubdomainsDefaultWithCustomConfig(t *testing.T) {
	// When user provides a partial config, HSTS still defaults to 2 years.
	// There is no zero-value trap: customizing other fields does NOT disable HSTS.
	mw := New(Config{
		ContentSecurityPolicy: "default-src 'self'",
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=63072000; includeSubDomains")
}

func TestHSTSExcludeSubdomains(t *testing.T) {
	mw := New(Config{
		HSTSMaxAge:            3600,
		HSTSExcludeSubdomains: true,
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=3600")
}

func TestHSTSPreload(t *testing.T) {
	mw := New(Config{
		HSTSMaxAge:  63072000,
		HSTSPreload: true,
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=63072000; includeSubDomains; preload")
}

func TestHSTSPreloadWithoutSubdomainsPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for HSTSPreload with HSTSExcludeSubdomains")
		}
	}()
	New(Config{
		HSTSMaxAge:            63072000,
		HSTSExcludeSubdomains: true,
		HSTSPreload:           true,
	})
}

func TestHSTSDisabled(t *testing.T) {
	mw := New(Config{HSTSMaxAge: -1})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "strict-transport-security")
}

func TestHSTSOnlyOnHTTPS(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}

	// HTTP request should not get HSTS.
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "strict-transport-security")

	// HTTPS via scheme override should get HSTS.
	rec, err = testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=63072000; includeSubDomains")
}

func TestCSP(t *testing.T) {
	mw := New(Config{ContentSecurityPolicy: "default-src 'self'"})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "content-security-policy", "default-src 'self'")
	testutil.AssertNoHeader(t, rec, "content-security-policy-report-only")
}

func TestCSPReportOnly(t *testing.T) {
	mw := New(Config{
		ContentSecurityPolicy: "default-src 'self'",
		CSPReportOnly:         true,
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "content-security-policy-report-only", "default-src 'self'")
	testutil.AssertNoHeader(t, rec, "content-security-policy")
}

func TestPermissionsPolicy(t *testing.T) {
	mw := New(Config{PermissionsPolicy: "geolocation=(), camera=()"})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "permissions-policy", "geolocation=(), camera=()")
}

func TestSkipFunction(t *testing.T) {
	mw := New(Config{
		Skip: func(_ *celeris.Context) bool { return true },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "x-content-type-options")
	testutil.AssertNoHeader(t, rec, "x-frame-options")
	testutil.AssertNoHeader(t, rec, "strict-transport-security")
}

func TestSkipFunctionSelectiveSkip(t *testing.T) {
	mw := New(Config{
		Skip: func(c *celeris.Context) bool { return c.Path() == "/health" },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-content-type-options")

	rec, err = testutil.RunChain(t, chain, "GET", "/api")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-content-type-options", "nosniff")
}

func TestSkipPaths(t *testing.T) {
	mw := New(Config{SkipPaths: []string{"/health", "/ready"}})
	chain := []celeris.HandlerFunc{mw, okHandler}

	rec, err := testutil.RunChain(t, chain, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-content-type-options")

	rec, err = testutil.RunChain(t, chain, "GET", "/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-content-type-options")

	rec, err = testutil.RunChain(t, chain, "GET", "/api")
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "x-content-type-options", "nosniff")
}

func TestApplyDefaultsFillsEmptyFields(t *testing.T) {
	cfg := applyDefaults(Config{})
	if cfg.XContentTypeOptions != "nosniff" {
		t.Fatalf("XContentTypeOptions: got %q, want %q", cfg.XContentTypeOptions, "nosniff")
	}
	if cfg.XFrameOptions != "SAMEORIGIN" {
		t.Fatalf("XFrameOptions: got %q, want %q", cfg.XFrameOptions, "SAMEORIGIN")
	}
	if cfg.XSSProtection != "0" {
		t.Fatalf("XSSProtection: got %q, want %q", cfg.XSSProtection, "0")
	}
	if cfg.HSTSMaxAge != 63072000 {
		t.Fatalf("HSTSMaxAge: got %d, want %d", cfg.HSTSMaxAge, 63072000)
	}
	if cfg.ReferrerPolicy != "strict-origin-when-cross-origin" {
		t.Fatalf("ReferrerPolicy: got %q, want %q", cfg.ReferrerPolicy, "strict-origin-when-cross-origin")
	}
	if cfg.CrossOriginOpenerPolicy != "same-origin" {
		t.Fatalf("CrossOriginOpenerPolicy: got %q, want %q", cfg.CrossOriginOpenerPolicy, "same-origin")
	}
	if cfg.CrossOriginResourcePolicy != "same-origin" {
		t.Fatalf("CrossOriginResourcePolicy: got %q, want %q", cfg.CrossOriginResourcePolicy, "same-origin")
	}
	if cfg.CrossOriginEmbedderPolicy != "require-corp" {
		t.Fatalf("CrossOriginEmbedderPolicy: got %q, want %q", cfg.CrossOriginEmbedderPolicy, "require-corp")
	}
	if cfg.XDNSPrefetchControl != "off" {
		t.Fatalf("XDNSPrefetchControl: got %q, want %q", cfg.XDNSPrefetchControl, "off")
	}
	if cfg.XPermittedCrossDomain != "none" {
		t.Fatalf("XPermittedCrossDomain: got %q, want %q", cfg.XPermittedCrossDomain, "none")
	}
	if cfg.OriginAgentCluster != "?1" {
		t.Fatalf("OriginAgentCluster: got %q, want %q", cfg.OriginAgentCluster, "?1")
	}
	if cfg.XDownloadOptions != "noopen" {
		t.Fatalf("XDownloadOptions: got %q, want %q", cfg.XDownloadOptions, "noopen")
	}
}

func TestApplyDefaultsPreservesCustomValues(t *testing.T) {
	cfg := applyDefaults(Config{
		XContentTypeOptions: "custom",
		XFrameOptions:       "DENY",
		HSTSMaxAge:          3600,
		ReferrerPolicy:      "no-referrer",
	})
	if cfg.XContentTypeOptions != "custom" {
		t.Fatalf("XContentTypeOptions: got %q, want %q", cfg.XContentTypeOptions, "custom")
	}
	if cfg.XFrameOptions != "DENY" {
		t.Fatalf("XFrameOptions: got %q, want %q", cfg.XFrameOptions, "DENY")
	}
	if cfg.HSTSMaxAge != 3600 {
		t.Fatalf("HSTSMaxAge: got %d, want %d", cfg.HSTSMaxAge, 3600)
	}
	if cfg.ReferrerPolicy != "no-referrer" {
		t.Fatalf("ReferrerPolicy: got %q, want %q", cfg.ReferrerPolicy, "no-referrer")
	}
}

func TestBuildHeadersSkipsEmptyStrings(t *testing.T) {
	cfg := Config{
		XContentTypeOptions:       "nosniff",
		XFrameOptions:             "",
		XSSProtection:             "",
		HSTSMaxAge:                -1,
		ContentSecurityPolicy:     "",
		ReferrerPolicy:            "",
		PermissionsPolicy:         "",
		CrossOriginOpenerPolicy:   "",
		CrossOriginResourcePolicy: "",
		CrossOriginEmbedderPolicy: "",
		XDNSPrefetchControl:       "",
		XPermittedCrossDomain:     "",
		OriginAgentCluster:        "",
		XDownloadOptions:          "",
	}
	headers := buildHeaders(cfg)
	if len(headers) != 1 {
		t.Fatalf("expected 1 header, got %d: %v", len(headers), headers)
	}
	if headers[0][0] != "x-content-type-options" || headers[0][1] != "nosniff" {
		t.Fatalf("unexpected header: %v", headers[0])
	}
}

func TestBuildHeadersDefaultCount(t *testing.T) {
	cfg := applyDefaults(Config{})
	headers := buildHeaders(cfg)
	// Default config should produce 11 headers (no CSP, no PermissionsPolicy).
	// x-content-type-options, x-frame-options, x-xss-protection,
	// referrer-policy, cross-origin-opener-policy, cross-origin-resource-policy,
	// cross-origin-embedder-policy, x-dns-prefetch-control, x-permitted-cross-domain-policies,
	// origin-agent-cluster, x-download-options.
	// Note: HSTS is not in buildHeaders (runtime check).
	if len(headers) != 11 {
		t.Fatalf("expected 11 headers from default config, got %d", len(headers))
	}
}

func TestBuildHeadersWithCSPAndPermissions(t *testing.T) {
	cfg := applyDefaults(Config{
		ContentSecurityPolicy: "default-src 'self'",
		PermissionsPolicy:     "camera=()",
	})
	headers := buildHeaders(cfg)
	// 11 defaults + CSP + PermissionsPolicy = 13.
	if len(headers) != 13 {
		t.Fatalf("expected 13 headers, got %d", len(headers))
	}
}

func TestNewNoArgs(t *testing.T) {
	mw := New()
	ctx, _ := celeristest.NewContextT(t, "GET", "/")
	err := mw(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := 0
	for _, h := range ctx.ResponseHeaders() {
		if h[0] == "x-content-type-options" && h[1] == "nosniff" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected x-content-type-options set once, found %d times", found)
	}
}

func TestMiddlewareCallsNext(t *testing.T) {
	mw := New()
	called := false
	handler := func(_ *celeris.Context) error {
		called = true
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	if !called {
		t.Fatal("expected downstream handler to be called")
	}
}

func TestMiddlewarePreservesHandlerResponse(t *testing.T) {
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestMiddlewareReturnsHandlerError(t *testing.T) {
	mw := New()
	errHandler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(500, "fail")
	}
	chain := []celeris.HandlerFunc{mw, errHandler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertHTTPError(t, err, 500)
}

func TestBuildHSTSValue(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"disabled negative", Config{HSTSMaxAge: -1}, ""},
		{"disabled flag", Config{DisableHSTS: true, HSTSMaxAge: 63072000}, ""},
		{"zero raw", Config{HSTSMaxAge: 0}, ""},
		{"with subdomains", Config{HSTSMaxAge: 3600}, "max-age=3600; includeSubDomains"},
		{"exclude subdomains", Config{HSTSMaxAge: 3600, HSTSExcludeSubdomains: true}, "max-age=3600"},
		{"with preload", Config{HSTSMaxAge: 3600, HSTSPreload: true}, "max-age=3600; includeSubDomains; preload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildHSTSValue(tt.cfg)
			if got != tt.want {
				t.Fatalf("buildHSTSValue: got %q, want %q", got, tt.want)
			}
		})
	}
}

// --- validate() panic tests ---

func TestValidatePanics(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"preload max-age too low", Config{HSTSMaxAge: 3600, HSTSPreload: true}},
		{"preload exclude subdomains", Config{HSTSMaxAge: 63072000, HSTSPreload: true, HSTSExcludeSubdomains: true}},
		{"CSPReportOnly without CSP", Config{CSPReportOnly: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			New(tt.cfg)
		})
	}
}

func TestValidateHSTSPreloadValidConfig(t *testing.T) {
	// Should not panic: max-age >= 1 year, includeSubDomains (default).
	mw := New(Config{
		HSTSMaxAge:  31536000,
		HSTSPreload: true,
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/",
		celeristest.WithScheme("https"))
	testutil.AssertNoError(t, err)
	testutil.AssertHeader(t, rec, "strict-transport-security", "max-age=31536000; includeSubDomains; preload")
}

// --- Suppress sentinel tests ---

func TestSuppressXFrameOptions(t *testing.T) {
	mw := New(Config{XFrameOptions: Suppress})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-frame-options")
	// Other defaults should still be present.
	testutil.AssertHeader(t, rec, "x-content-type-options", "nosniff")
}

func TestSuppressMultipleHeaders(t *testing.T) {
	mw := New(Config{
		XFrameOptions:       Suppress,
		XSSProtection:       Suppress,
		XDNSPrefetchControl: Suppress,
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertNoHeader(t, rec, "x-frame-options")
	testutil.AssertNoHeader(t, rec, "x-xss-protection")
	testutil.AssertNoHeader(t, rec, "x-dns-prefetch-control")
	testutil.AssertHeader(t, rec, "x-content-type-options", "nosniff")
	testutil.AssertHeader(t, rec, "referrer-policy", "strict-origin-when-cross-origin")
}

func TestSuppressNotOverriddenByDefaults(t *testing.T) {
	// Suppress is not an empty string, so applyDefaults should NOT override it.
	cfg := applyDefaults(Config{XContentTypeOptions: Suppress})
	if cfg.XContentTypeOptions != Suppress {
		t.Fatalf("expected Suppress sentinel to survive applyDefaults, got %q", cfg.XContentTypeOptions)
	}
}

// --- YAML struct tag tests ---

func TestConfigYAMLTags(t *testing.T) {
	expected := map[string]string{
		"Skip":                      "-",
		"SkipPaths":                 "skip_paths",
		"XContentTypeOptions":       "x_content_type_options",
		"XFrameOptions":             "x_frame_options",
		"XSSProtection":             "xss_protection",
		"HSTSMaxAge":                "hsts_max_age",
		"HSTSExcludeSubdomains":     "hsts_exclude_subdomains",
		"HSTSPreload":               "hsts_preload",
		"ContentSecurityPolicy":     "content_security_policy",
		"CSPReportOnly":             "csp_report_only",
		"ReferrerPolicy":            "referrer_policy",
		"PermissionsPolicy":         "permissions_policy",
		"CrossOriginOpenerPolicy":   "cross_origin_opener_policy",
		"CrossOriginResourcePolicy": "cross_origin_resource_policy",
		"CrossOriginEmbedderPolicy": "cross_origin_embedder_policy",
		"XDNSPrefetchControl":       "x_dns_prefetch_control",
		"XPermittedCrossDomain":     "x_permitted_cross_domain",
		"OriginAgentCluster":        "origin_agent_cluster",
		"XDownloadOptions":          "x_download_options",
	}

	typ := reflect.TypeOf(Config{})
	for fieldName, wantTag := range expected {
		field, ok := typ.FieldByName(fieldName)
		if !ok {
			t.Fatalf("field %q not found in Config", fieldName)
		}
		gotTag := field.Tag.Get("yaml")
		if gotTag != wantTag {
			t.Fatalf("Config.%s yaml tag: got %q, want %q", fieldName, gotTag, wantTag)
		}
	}
}

func TestConfigYAMLTagsAllFieldsCovered(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" {
			t.Fatalf("Config.%s is missing a yaml struct tag", field.Name)
		}
	}
}
