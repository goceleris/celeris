package bodylimit

import (
	"errors"
	"strconv"
	"strings"

	"github.com/goceleris/celeris"
)

// Config defines the body limit middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// Limit is a human-readable size string (e.g., "10MB", "1.5GB").
	// Takes precedence over MaxBytes when set.
	Limit string

	// MaxBytes is the maximum allowed request body size in bytes.
	// Default: 4 MB (4 * 1024 * 1024).
	MaxBytes int64

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// ErrorHandler handles body-too-large errors. When non-nil, it is
	// called instead of returning ErrBodyTooLarge directly.
	ErrorHandler func(c *celeris.Context, err error) error

	// ContentLengthRequired rejects requests that lack a Content-Length
	// header with 411 Length Required. Enable this to force clients to
	// declare body size up front, preventing memory exhaustion from
	// unbounded streaming when the framework buffers the full body
	// before middleware runs.
	ContentLengthRequired bool
}

var defaultConfig = Config{
	MaxBytes: 4 * 1024 * 1024, // 4 MB
}

func applyDefaults(cfg Config) Config {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultConfig.MaxBytes
	}
	return cfg
}

func validate(cfg *Config) {
	if cfg.Limit != "" {
		n, err := parseSize(cfg.Limit)
		if err != nil {
			panic(err.Error())
		}
		cfg.MaxBytes = n
	}
}

type sizeSuffix struct {
	name string
	mult float64
}

// Order matters: longer suffixes must be checked before shorter ones
// (e.g., "EIB" before "EB", "EB" before "B").
var sizeSuffixes = [...]sizeSuffix{
	{"EIB", 1 << 60},
	{"PIB", 1 << 50},
	{"TIB", 1 << 40},
	{"GIB", 1 << 30},
	{"MIB", 1 << 20},
	{"KIB", 1 << 10},
	{"EB", 1 << 60},
	{"PB", 1 << 50},
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"KB", 1 << 10},
	{"B", 1},
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("bodylimit: empty Limit string")
	}

	upper := strings.ToUpper(s)
	for _, sf := range sizeSuffixes {
		if strings.HasSuffix(upper, sf.name) {
			numStr := strings.TrimSpace(s[:len(s)-len(sf.name)])
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, errors.New("bodylimit: invalid Limit: " + s)
			}
			if val < 0 {
				return 0, errors.New("bodylimit: Limit must not be negative")
			}
			n := int64(val * sf.mult)
			if n <= 0 {
				return 0, errors.New("bodylimit: Limit must be positive: " + s)
			}
			return n, nil
		}
	}
	return 0, errors.New("bodylimit: invalid Limit format (use B, KB, MB, GB, TB, PB, EB or KiB, MiB, GiB, TiB, PiB, EiB): " + s)
}
