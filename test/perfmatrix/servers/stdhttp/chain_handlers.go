package stdhttp

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// mountChainHandlers mounts the 4 middleware chains (api, auth, security,
// fullstack) under /chain/<name>/json and /chain/<name>/upload on s. Each
// chain wraps the same 2 terminal handlers with a different middleware
// stack so scenarios differ only by middleware depth, matching the 4×3
// benchmark matrix.
//
// Since net/http has no first-class middleware, each piece is a plain
// http.Handler decorator. Correctness is prioritised over feature parity
// with celeris's middleware — the chains exist to measure overhead, not
// to reimplement the middleware ecosystem for 5 frameworks.
func mountChainHandlers(s *Server) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.mountedChain {
		s.mu.Unlock()
		return
	}
	s.mountedChain = true
	s.mu.Unlock()

	jsonTerminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.jsonSmall)
	})
	uploadTerminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("OK"))
	})

	for _, spec := range []struct {
		prefix string
		wrap   func(http.Handler) http.Handler
	}{
		{"/chain/api/", chainAPI},
		{"/chain/auth/", chainAuth},
		{"/chain/security/", chainSecurity},
		{"/chain/fullstack/", chainFullstack},
	} {
		s.Mount(http.MethodGet, spec.prefix+"json", spec.wrap(jsonTerminal))
		s.Mount(http.MethodPost, spec.prefix+"upload", spec.wrap(uploadTerminal))
	}
}

// chainAPI wraps h with requestid + logger + recovery + cors.
func chainAPI(h http.Handler) http.Handler {
	return mwRequestID(mwLoggerDiscard(mwRecovery(mwCORS(h))))
}

// chainAuth is api + basicauth.
func chainAuth(h http.Handler) http.Handler {
	return chainAPI(mwBasicAuth(h, "bench", "bench"))
}

// chainSecurity is auth + csrf (skipped during bench) + secure.
func chainSecurity(h http.Handler) http.Handler {
	return chainAuth(mwCSRFSkip(mwSecure(h)))
}

// chainFullstack is security + ratelimit + timeout + bodylimit.
func chainFullstack(h http.Handler) http.Handler {
	return chainSecurity(mwRateLimit(mwTimeoutDummy(mwBodyLimit(h, 10<<20))))
}

// mwRequestID attaches X-Request-Id to the response, reusing the inbound
// header when present. Uses google/uuid to match the well-known
// competitor stacks.
func mwRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

// mwLoggerDiscard runs the framework-equivalent of a logging middleware
// but writes to io.Discard so bench output is not polluted. We retain
// the header-capture cost (c.Request.Method + path lookup) so the
// overhead measurement remains realistic.
func mwLoggerDiscard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the minimal work a logger middleware does: read a
		// handful of request fields and format a one-liner. The string
		// is written to io.Discard so the format/formatter cost is real
		// but no output is produced.
		_, _ = io.WriteString(io.Discard, r.Method+" "+r.URL.Path+"\n")
		next.ServeHTTP(w, r)
	})
}

// mwRecovery defers recover() on the downstream handler.
func mwRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// mwCORS writes the basic CORS allow-all headers. Matches what
// github.com/rs/cors would emit for an unconfigured request; rolled
// inline to avoid the dep.
func mwCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// basicAuthHeaderPrefix is the case-sensitive prefix we match on the
// Authorization header. The verification uses constant-time compare so
// timing side-channels do not leak the expected credential.
const basicAuthHeaderPrefix = "Basic "

// mwBasicAuth enforces user:pass on the inbound Authorization header.
// A constant-time compare prevents timing side-channels leaking the
// credential.
func mwBasicAuth(next http.Handler, user, pass string) http.Handler {
	expect := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	expectBytes := []byte(expect)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, basicAuthHeaderPrefix) {
			w.Header().Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(auth[len(basicAuthHeaderPrefix):])
		if subtle.ConstantTimeCompare(got, expectBytes) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mwCSRFSkip mimics a CSRF middleware's hot-path cost without actually
// rejecting requests. For a benchmark we care about the token-generation
// + cookie-emit path, not the validation reject (which loadgen can't
// feed anyway). See README: "CSRF is hard to mock cleanly in a bench".
func mwCSRFSkip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Emit a dummy CSRF cookie on every response to approximate the
		// header-write cost a real CSRF middleware would incur.
		http.SetCookie(w, &http.Cookie{
			Name:     "_csrf",
			Value:    "skip-token-bench",
			Path:     "/",
			HttpOnly: true,
		})
		next.ServeHTTP(w, r)
	})
}

// mwSecure emits the OWASP recommended security headers. Mirrors what
// github.com/unrolled/secure would emit in a default configuration;
// hand-rolled to keep the transitive dep count down.
func mwSecure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-XSS-Protection", "0")
		next.ServeHTTP(w, r)
	})
}

// chainLimiter is the package-wide token bucket shared across all
// /chain/fullstack/* requests. Uses golang.org/x/time/rate — the same
// algorithm celeris's middleware ships — so the overhead comparison is
// fair. Rate is set high enough that no bench rig trips the limit; we
// measure the per-request Allow() cost, not actual dropping.
var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

// mwRateLimit rejects when the global limiter is saturated.
func mwRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !chainLimiter.Allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mwTimeoutDummy wraps next with a 30-second context deadline. We do not
// use http.TimeoutHandler because it double-serves headers on H2C
// upgrade paths; the deadline is propagated via context which the
// handlers we wrap simply ignore (static endpoints complete faster than
// any realistic deadline).
func mwTimeoutDummy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// mwBodyLimit rejects requests exceeding maxBytes using the stdlib
// MaxBytesReader, which is the canonical way to cap request body size in
// net/http.
func mwBodyLimit(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

// chainMWOrderDoc records the authoritative middleware ordering per
// chain. Used by tests and the report table to stay in sync.
var chainMWOrderDoc = map[string][]string{
	"api":       {"requestid", "logger", "recovery", "cors"},
	"auth":      {"requestid", "logger", "recovery", "cors", "basicauth"},
	"security":  {"requestid", "logger", "recovery", "cors", "basicauth", "csrf", "secure"},
	"fullstack": {"requestid", "logger", "recovery", "cors", "basicauth", "csrf", "secure", "ratelimit", "timeout", "bodylimit"},
}

// Compile-time "unused" guard for chainMWOrderDoc (read by tests in the
// perfmatrix/report package).
var _ = chainMWOrderDoc
