package methodoverride

import (
	"strings"

	"github.com/goceleris/celeris"
)

const (
	// DefaultHeader is the default HTTP header checked for method override.
	DefaultHeader = "X-HTTP-Method-Override"

	// DefaultFormField is the default form field checked for method override.
	DefaultFormField = "_method"
)

// Config defines the method override middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match on c.Path()).
	SkipPaths []string

	// AllowedMethods lists the original HTTP methods eligible for override.
	// Default: ["POST"]. Must not contain empty or whitespace-only strings.
	AllowedMethods []string

	// Getter extracts the override method from the request. The returned
	// string should be an HTTP method name (e.g. "PUT", "DELETE"). An
	// empty return value means no override.
	// Default: [defaultGetter] (checks form field [DefaultFormField] first,
	// then header [DefaultHeader]).
	Getter func(c *celeris.Context) string
}

// defaultConfig is the default method override configuration.
var defaultConfig = Config{
	AllowedMethods: []string{"POST"},
}

func applyDefaults(cfg Config) Config {
	if len(cfg.AllowedMethods) == 0 {
		cfg.AllowedMethods = defaultConfig.AllowedMethods
	}
	if cfg.Getter == nil {
		cfg.Getter = defaultGetter
	}
	return cfg
}

func validate(cfg Config) {
	for _, m := range cfg.AllowedMethods {
		if strings.TrimSpace(m) == "" {
			panic("methodoverride: AllowedMethods must not contain empty or whitespace-only strings")
		}
	}
}

// defaultGetter checks the form field DefaultFormField first, then the
// header DefaultHeader. This order allows HTML forms (which cannot set
// custom headers) to override the method without client-side JavaScript.
func defaultGetter(c *celeris.Context) string {
	if m := c.FormValue(DefaultFormField); m != "" {
		return m
	}
	return c.Header(strings.ToLower(DefaultHeader))
}

// HeaderGetter returns a getter that reads the override method from the
// given HTTP header. The header name is lowercased for lookup.
func HeaderGetter(header string) func(*celeris.Context) string {
	h := strings.ToLower(header)
	return func(c *celeris.Context) string {
		return c.Header(h)
	}
}

// FormFieldGetter returns a getter that reads the override method from the
// given form field.
func FormFieldGetter(field string) func(*celeris.Context) string {
	return func(c *celeris.Context) string {
		return c.FormValue(field)
	}
}

// FormThenHeaderGetter checks the form field first, then the header.
// This matches the default getter order with custom field/header names.
func FormThenHeaderGetter(field, header string) func(*celeris.Context) string {
	h := strings.ToLower(header)
	return func(c *celeris.Context) string {
		if m := c.FormValue(field); m != "" {
			return m
		}
		return c.Header(h)
	}
}
