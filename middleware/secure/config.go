package secure

import (
	"strconv"

	"github.com/goceleris/celeris"
)

// Config defines the security headers middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool `yaml:"-"`

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string `yaml:"skip_paths"`

	// XContentTypeOptions sets the X-Content-Type-Options header.
	// Default: "nosniff".
	XContentTypeOptions string `yaml:"x_content_type_options"`

	// XFrameOptions sets the X-Frame-Options header.
	// Default: "SAMEORIGIN".
	XFrameOptions string `yaml:"x_frame_options"`

	// XSSProtection sets the X-XSS-Protection header.
	// Default: "0" (disables the XSS auditor, per modern best practice).
	XSSProtection string `yaml:"xss_protection"`

	// HSTSMaxAge sets the max-age directive of Strict-Transport-Security in seconds.
	// Default: 63072000 (2 years). Use [Config].DisableHSTS to omit the header.
	HSTSMaxAge int `yaml:"hsts_max_age"`

	// DisableHSTS omits the Strict-Transport-Security header entirely.
	// Default: false (HSTS is enabled with HSTSMaxAge).
	DisableHSTS bool `yaml:"disable_hsts"`

	// HSTSExcludeSubdomains opts out of includeSubDomains in the HSTS header.
	// By default includeSubDomains is included. Set to true to remove it.
	HSTSExcludeSubdomains bool `yaml:"hsts_exclude_subdomains"`

	// HSTSPreload adds preload to the HSTS header.
	// Default: false.
	HSTSPreload bool `yaml:"hsts_preload"`

	// ContentSecurityPolicy sets the Content-Security-Policy header.
	// Default: "" (omitted).
	ContentSecurityPolicy string `yaml:"content_security_policy"`

	// CSPReportOnly uses Content-Security-Policy-Report-Only instead of
	// Content-Security-Policy when true. Default: false.
	CSPReportOnly bool `yaml:"csp_report_only"`

	// ReferrerPolicy sets the Referrer-Policy header.
	// Default: "strict-origin-when-cross-origin".
	ReferrerPolicy string `yaml:"referrer_policy"`

	// PermissionsPolicy sets the Permissions-Policy header.
	// Default: "" (omitted).
	PermissionsPolicy string `yaml:"permissions_policy"`

	// CrossOriginOpenerPolicy sets the Cross-Origin-Opener-Policy header.
	// Default: "same-origin".
	CrossOriginOpenerPolicy string `yaml:"cross_origin_opener_policy"`

	// CrossOriginResourcePolicy sets the Cross-Origin-Resource-Policy header.
	// Default: "same-origin".
	CrossOriginResourcePolicy string `yaml:"cross_origin_resource_policy"`

	// CrossOriginEmbedderPolicy sets the Cross-Origin-Embedder-Policy header.
	// Default: "require-corp".
	CrossOriginEmbedderPolicy string `yaml:"cross_origin_embedder_policy"`

	// XDNSPrefetchControl sets the X-DNS-Prefetch-Control header.
	// Default: "off".
	XDNSPrefetchControl string `yaml:"x_dns_prefetch_control"`

	// XPermittedCrossDomain sets the X-Permitted-Cross-Domain-Policies header.
	// Default: "none".
	XPermittedCrossDomain string `yaml:"x_permitted_cross_domain"`

	// OriginAgentCluster sets the Origin-Agent-Cluster header.
	// Default: "?1".
	OriginAgentCluster string `yaml:"origin_agent_cluster"`

	// XDownloadOptions sets the X-Download-Options header.
	// Default: "noopen".
	XDownloadOptions string `yaml:"x_download_options"`
}

// defaultConfig is the default security headers configuration.
// It is unexported to prevent mutation; use New() with no arguments
// for the default behavior.
var defaultConfig = Config{
	XContentTypeOptions:       "nosniff",
	XFrameOptions:             "SAMEORIGIN",
	XSSProtection:             "0",
	HSTSMaxAge:                63072000,
	ReferrerPolicy:            "strict-origin-when-cross-origin",
	CrossOriginOpenerPolicy:   "same-origin",
	CrossOriginResourcePolicy: "same-origin",
	CrossOriginEmbedderPolicy: "require-corp",
	XDNSPrefetchControl:       "off",
	XPermittedCrossDomain:     "none",
	OriginAgentCluster:        "?1",
	XDownloadOptions:          "noopen",
}

func applyDefaults(cfg Config) Config {
	if cfg.XContentTypeOptions == "" {
		cfg.XContentTypeOptions = defaultConfig.XContentTypeOptions
	}
	if cfg.XFrameOptions == "" {
		cfg.XFrameOptions = defaultConfig.XFrameOptions
	}
	if cfg.XSSProtection == "" {
		cfg.XSSProtection = defaultConfig.XSSProtection
	}
	if cfg.HSTSMaxAge == 0 && !cfg.DisableHSTS {
		cfg.HSTSMaxAge = defaultConfig.HSTSMaxAge
	}
	if cfg.ReferrerPolicy == "" {
		cfg.ReferrerPolicy = defaultConfig.ReferrerPolicy
	}
	if cfg.CrossOriginOpenerPolicy == "" {
		cfg.CrossOriginOpenerPolicy = defaultConfig.CrossOriginOpenerPolicy
	}
	if cfg.CrossOriginResourcePolicy == "" {
		cfg.CrossOriginResourcePolicy = defaultConfig.CrossOriginResourcePolicy
	}
	if cfg.CrossOriginEmbedderPolicy == "" {
		cfg.CrossOriginEmbedderPolicy = defaultConfig.CrossOriginEmbedderPolicy
	}
	if cfg.XDNSPrefetchControl == "" {
		cfg.XDNSPrefetchControl = defaultConfig.XDNSPrefetchControl
	}
	if cfg.XPermittedCrossDomain == "" {
		cfg.XPermittedCrossDomain = defaultConfig.XPermittedCrossDomain
	}
	if cfg.OriginAgentCluster == "" {
		cfg.OriginAgentCluster = defaultConfig.OriginAgentCluster
	}
	if cfg.XDownloadOptions == "" {
		cfg.XDownloadOptions = defaultConfig.XDownloadOptions
	}
	return cfg
}

func shouldEmit(v string) bool {
	return v != "" && v != Suppress
}

// Suppress is a sentinel value that, when set on a header field in [Config],
// causes that header to be omitted from the response entirely. Unlike an
// empty string (which is overridden by the default), Suppress explicitly
// opts out of a header after defaults have been applied.
const Suppress = "-"

func (cfg Config) validate() {
	if cfg.HSTSPreload && cfg.HSTSMaxAge > 0 && cfg.HSTSMaxAge < 31536000 {
		panic("secure: HSTSPreload requires HSTSMaxAge >= 31536000 (1 year)")
	}
	if cfg.HSTSPreload && cfg.HSTSExcludeSubdomains {
		panic("secure: HSTSPreload requires includeSubDomains (HSTSExcludeSubdomains must be false)")
	}
	if cfg.CSPReportOnly && cfg.ContentSecurityPolicy == "" {
		panic("secure: CSPReportOnly requires a non-empty ContentSecurityPolicy")
	}
	if cfg.CSPReportOnly && cfg.ContentSecurityPolicy == Suppress {
		panic("secure: CSPReportOnly with ContentSecurityPolicy=Suppress is a no-op; set CSPReportOnly=false or provide a policy")
	}
}

// buildHeaders pre-computes the header key-value pairs from the config.
// Empty string values are excluded. HSTS is excluded here because it
// requires a runtime HTTPS check; see secure.go.
func buildHeaders(cfg Config) [][2]string {
	headers := make([][2]string, 0, 13)

	if shouldEmit(cfg.XContentTypeOptions) {
		headers = append(headers, [2]string{"x-content-type-options", cfg.XContentTypeOptions})
	}
	if shouldEmit(cfg.XFrameOptions) {
		headers = append(headers, [2]string{"x-frame-options", cfg.XFrameOptions})
	}
	if shouldEmit(cfg.XSSProtection) {
		headers = append(headers, [2]string{"x-xss-protection", cfg.XSSProtection})
	}
	if shouldEmit(cfg.ContentSecurityPolicy) {
		key := "content-security-policy"
		if cfg.CSPReportOnly {
			key = "content-security-policy-report-only"
		}
		headers = append(headers, [2]string{key, cfg.ContentSecurityPolicy})
	}
	if shouldEmit(cfg.ReferrerPolicy) {
		headers = append(headers, [2]string{"referrer-policy", cfg.ReferrerPolicy})
	}
	if shouldEmit(cfg.PermissionsPolicy) {
		headers = append(headers, [2]string{"permissions-policy", cfg.PermissionsPolicy})
	}
	if shouldEmit(cfg.CrossOriginOpenerPolicy) {
		headers = append(headers, [2]string{"cross-origin-opener-policy", cfg.CrossOriginOpenerPolicy})
	}
	if shouldEmit(cfg.CrossOriginResourcePolicy) {
		headers = append(headers, [2]string{"cross-origin-resource-policy", cfg.CrossOriginResourcePolicy})
	}
	if shouldEmit(cfg.CrossOriginEmbedderPolicy) {
		headers = append(headers, [2]string{"cross-origin-embedder-policy", cfg.CrossOriginEmbedderPolicy})
	}
	if shouldEmit(cfg.XDNSPrefetchControl) {
		headers = append(headers, [2]string{"x-dns-prefetch-control", cfg.XDNSPrefetchControl})
	}
	if shouldEmit(cfg.XPermittedCrossDomain) {
		headers = append(headers, [2]string{"x-permitted-cross-domain-policies", cfg.XPermittedCrossDomain})
	}
	if shouldEmit(cfg.OriginAgentCluster) {
		headers = append(headers, [2]string{"origin-agent-cluster", cfg.OriginAgentCluster})
	}
	if shouldEmit(cfg.XDownloadOptions) {
		headers = append(headers, [2]string{"x-download-options", cfg.XDownloadOptions})
	}

	return headers
}

// buildHSTSValue pre-computes the HSTS header value. Returns empty string
// when HSTS is disabled (HSTSMaxAge <= 0).
func buildHSTSValue(cfg Config) string {
	if cfg.DisableHSTS || cfg.HSTSMaxAge <= 0 {
		return ""
	}
	v := "max-age=" + strconv.Itoa(cfg.HSTSMaxAge)
	if !cfg.HSTSExcludeSubdomains {
		v += "; includeSubDomains"
	}
	if cfg.HSTSPreload {
		v += "; preload"
	}
	return v
}
