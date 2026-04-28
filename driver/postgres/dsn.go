package postgres

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Options holds client-side driver knobs parsed out of the DSN before they are
// passed to the server as startup parameters.
type Options struct {
	// ConnectTimeout bounds dial + startup. Zero means no client-side timeout.
	ConnectTimeout time.Duration
	// StatementCacheSize is the per-conn LRU capacity for named prepared
	// statements. Zero disables the cache.
	StatementCacheSize int
	// AutoCacheStatements, when true AND StatementCacheSize > 0, causes
	// cacheable SELECT-style QueryContext calls to transparently auto-
	// prepare on first use and reuse via the extended protocol on
	// subsequent calls. Equivalent to pgx's QueryExecModeCacheStatement
	// at steady state. Default true; opt out per DSN with
	// auto_cache_statements=false (preserves the simple-query behaviour
	// for callers that do not want per-conn statement caching).
	AutoCacheStatements bool
	// autoCacheStatementsExplicit tracks whether the DSN parser observed
	// an explicit auto_cache_statements=… parameter so applyDefaults can
	// distinguish "user opted out" from "field was never set" before
	// flipping the default to true.
	autoCacheStatementsExplicit bool
	// Application is copied into the "application_name" startup parameter.
	Application string
	// SSLMode is the parsed sslmode value. "disable" and "prefer" are
	// accepted; "require" / "verify-ca" / "verify-full" are rejected at Open
	// time with ErrSSLNotSupported.
	SSLMode string
}

// DSN is the parsed form of a PostgreSQL connection string.
type DSN struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	// Params is the set of server-side startup parameters (e.g. "search_path",
	// "timezone") that will be sent in the StartupMessage.
	Params  map[string]string
	Options Options
}

const (
	defaultHost    = "localhost"
	defaultPort    = "5432"
	defaultUser    = "postgres"
	defaultCacheSz = 256
)

// ParseDSN parses either the URL form ("postgres://user:pass@host:port/db?...")
// or the key=value form ("host=... port=... user=...") and returns a populated
// DSN. Unknown keys are carried as server-side startup parameters rather than
// rejected; the PG server validates them during the StartupMessage exchange.
func ParseDSN(raw string) (DSN, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DSN{}, errors.New("celeris-postgres: empty DSN")
	}
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		return parseURLDSN(raw)
	}
	return parseKVDSN(raw)
}

func parseURLDSN(raw string) (DSN, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return DSN{}, fmt.Errorf("celeris-postgres: parse DSN url: %w", err)
	}
	d := DSN{Params: map[string]string{}}
	if u.User != nil {
		d.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			d.Password = pw
		}
	}
	host := u.Hostname()
	port := u.Port()
	d.Host = host
	d.Port = port
	d.Database = strings.TrimPrefix(u.Path, "/")
	for k, vs := range u.Query() {
		if len(vs) == 0 {
			continue
		}
		if err := applyDSNKey(&d, k, vs[0]); err != nil {
			return DSN{}, err
		}
	}
	applyDefaults(&d)
	return d, nil
}

func parseKVDSN(raw string) (DSN, error) {
	d := DSN{Params: map[string]string{}}
	tokens, err := splitKV(raw)
	if err != nil {
		return DSN{}, err
	}
	for _, tok := range tokens {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			return DSN{}, fmt.Errorf("celeris-postgres: malformed DSN token %q", tok)
		}
		k := strings.TrimSpace(tok[:eq])
		v := tok[eq+1:]
		if err := applyDSNKey(&d, k, v); err != nil {
			return DSN{}, err
		}
	}
	applyDefaults(&d)
	return d, nil
}

// splitKV breaks "a=1 b='2 3' c=\"4\"" into tokens, handling single and double
// quoted values with backslash escapes.
func splitKV(raw string) ([]string, error) {
	var out []string
	var cur strings.Builder
	i := 0
	n := len(raw)
	for i < n {
		// Skip whitespace between tokens.
		for i < n && (raw[i] == ' ' || raw[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		cur.Reset()
		// Key up to '='.
		for i < n && raw[i] != '=' {
			cur.WriteByte(raw[i])
			i++
		}
		if i >= n {
			return nil, fmt.Errorf("celeris-postgres: DSN key without value: %q", cur.String())
		}
		// Consume '='.
		cur.WriteByte(raw[i])
		i++
		// Value: quoted or bare.
		if i < n && (raw[i] == '\'' || raw[i] == '"') {
			q := raw[i]
			i++
			for i < n && raw[i] != q {
				if raw[i] == '\\' && i+1 < n {
					i++
				}
				cur.WriteByte(raw[i])
				i++
			}
			if i >= n {
				return nil, errors.New("celeris-postgres: unterminated quoted DSN value")
			}
			i++ // closing quote
		} else {
			for i < n && raw[i] != ' ' && raw[i] != '\t' {
				cur.WriteByte(raw[i])
				i++
			}
		}
		out = append(out, cur.String())
	}
	return out, nil
}

// applyDSNKey routes a single key=value pair to the appropriate DSN field.
// Unknown keys are preserved as startup parameters so callers can pass
// arbitrary PG GUCs (search_path, statement_timeout, etc.) through the DSN.
func applyDSNKey(d *DSN, k, v string) error {
	switch strings.ToLower(k) {
	case "host":
		d.Host = v
	case "port":
		d.Port = v
	case "user":
		d.User = v
	case "password":
		d.Password = v
	case "dbname", "database":
		d.Database = v
	case "sslmode":
		d.Options.SSLMode = v
	case "connect_timeout":
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("celeris-postgres: connect_timeout = %q: %w", v, err)
		}
		d.Options.ConnectTimeout = time.Duration(n) * time.Second
	case "statement_cache_size":
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("celeris-postgres: statement_cache_size = %q: %w", v, err)
		}
		if n < 0 {
			return fmt.Errorf("celeris-postgres: statement_cache_size must be >= 0")
		}
		d.Options.StatementCacheSize = n
	case "auto_cache_statements":
		switch strings.ToLower(v) {
		case "1", "true", "on", "yes":
			d.Options.AutoCacheStatements = true
		case "0", "false", "off", "no":
			d.Options.AutoCacheStatements = false
		default:
			return fmt.Errorf("celeris-postgres: auto_cache_statements = %q: want bool", v)
		}
		d.Options.autoCacheStatementsExplicit = true
	case "application_name":
		d.Options.Application = v
	default:
		d.Params[k] = v
	}
	return nil
}

func applyDefaults(d *DSN) {
	if d.Host == "" {
		d.Host = defaultHost
	}
	if d.Port == "" {
		d.Port = defaultPort
	}
	if d.User == "" {
		d.User = defaultUser
	}
	if d.Options.StatementCacheSize == 0 {
		d.Options.StatementCacheSize = defaultCacheSz
	}
	if d.Options.SSLMode == "" {
		d.Options.SSLMode = "disable"
	}
	if d.Options.Application != "" {
		if d.Params == nil {
			d.Params = map[string]string{}
		}
		d.Params["application_name"] = d.Options.Application
	}
}

// CheckSSL returns ErrSSLNotSupported if the DSN requests a TLS mode this
// driver version cannot satisfy.
//
// In v1.4.0 the driver has no TLS stack. sslmode semantics:
//
//   - "" / "disable"         : plaintext, always allowed.
//   - "prefer" / "allow"     : the libpq semantics are "try TLS, fall
//     back to plaintext" (prefer) or "try plaintext,
//     upgrade if the server demands TLS" (allow). Since
//     we can only ever do plaintext, these are treated
//     as equivalent to "disable" but WITH a warning —
//     the user expected opportunistic encryption and
//     got none.
//   - "require" / "verify-ca" / "verify-full" : TLS is mandatory;
//     rejected with ErrSSLNotSupported. Users with managed
//     cloud DBs (RDS, CloudSQL, etc.) must wait for v1.4.x
//     TLS support.
func (d *DSN) CheckSSL() error {
	switch strings.ToLower(d.Options.SSLMode) {
	case "", "disable":
		return nil
	case "prefer", "allow":
		// Silent downgrade is a real operational footgun. We accept
		// the connection but emit a single stderr warning so callers
		// see the mismatch at startup instead of discovering it in
		// production.
		fmt.Fprintf(os.Stderr,
			"celeris-postgres: sslmode=%s silently downgraded to plaintext — v1.4.0 has no TLS stack (use sslmode=disable explicitly, or wait for v1.4.x TLS support)\n",
			d.Options.SSLMode)
		return nil
	case "require", "verify-ca", "verify-full":
		return fmt.Errorf("%w (sslmode=%s)", ErrSSLNotSupported, d.Options.SSLMode)
	default:
		return fmt.Errorf("celeris-postgres: unknown sslmode=%q", d.Options.SSLMode)
	}
}
