package echo

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	echov4 "github.com/labstack/echo/v4"
	"golang.org/x/time/rate"
)

// mountChainHandlers mounts the 4 middleware chains under
// /chain/<name>/json and /chain/<name>/upload. Echo's own middleware
// framework is used for the parts it covers natively; the two
// middlewares echo does not ship cleanly (csrf, secure) are hand-rolled
// as net/http decorators and bridged through echov4.WrapHandler so the
// overhead comparison remains fair.
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.jsonSmall)
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
		s.e.GET(spec.prefix+"json", echov4.WrapHandler(spec.wrap(jsonTerminal)))
		s.e.POST(spec.prefix+"upload", echov4.WrapHandler(spec.wrap(uploadTerminal)))
	}
}

// Chain decorators: identical structure to the other net/http-based
// packages; see stdhttp/chain_handlers.go for the annotated reference.
func chainAPI(h http.Handler) http.Handler {
	return mwRequestID(mwLoggerDiscard(mwRecovery(mwCORS(h))))
}
func chainAuth(h http.Handler) http.Handler {
	return chainAPI(mwBasicAuth(h, "bench", "bench"))
}
func chainSecurity(h http.Handler) http.Handler {
	return chainAuth(mwCSRFSkip(mwSecure(h)))
}
func chainFullstack(h http.Handler) http.Handler {
	return chainSecurity(mwRateLimit(mwTimeoutDummy(mwBodyLimit(h, 10<<20))))
}

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
func mwLoggerDiscard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(io.Discard, r.Method+" "+r.URL.Path+"\n")
		next.ServeHTTP(w, r)
	})
}
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

const basicAuthHeaderPrefix = "Basic "

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

// mwCSRFSkip: hand-rolled CSRF hot-path stand-in. Echo's csrf
// middleware ships with the framework but requires token lookup from
// form data loadgen cannot fake, so the overhead is approximated with
// a cookie-emit-only path that mirrors the happy flow.
func mwCSRFSkip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "skip-token-bench", Path: "/", HttpOnly: true})
		next.ServeHTTP(w, r)
	})
}

// mwSecure emits the OWASP headers; equivalent to unrolled/secure in
// default configuration. Rolled inline to avoid the dep.
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

var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func mwRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !chainLimiter.Allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func mwTimeoutDummy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func mwBodyLimit(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}
