package fasthttp

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"golang.org/x/time/rate"
)

// mountChainHandlers mounts the 4 middleware chains as native fasthttp
// handlers. Each chain hand-rolls the middleware as a RequestHandler
// decorator (≤ 20 lines each per the task spec) — fasthttp has no
// middleware framework, and faithful correctness of the decorator
// chain is more important than feature parity with fiber/hertz for this
// bench.
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

	jsonTerminal := func(rc *fasthttp.RequestCtx) {
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(s.jsonSmall)
	}
	uploadTerminal := func(rc *fasthttp.RequestCtx) {
		_ = rc.PostBody()
		rc.SetContentType("text/plain; charset=utf-8")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write([]byte("OK"))
	}

	for _, spec := range []struct {
		prefix string
		wrap   func(fasthttp.RequestHandler) fasthttp.RequestHandler
	}{
		{"/chain/api/", chainAPI},
		{"/chain/auth/", chainAuth},
		{"/chain/security/", chainSecurity},
		{"/chain/fullstack/", chainFullstack},
	} {
		s.MountNative(http.MethodGet, spec.prefix+"json", spec.wrap(jsonTerminal))
		s.MountNative(http.MethodPost, spec.prefix+"upload", spec.wrap(uploadTerminal))
	}
}

func chainAPI(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return mwRequestID(mwLoggerDiscard(mwRecovery(mwCORS(h))))
}
func chainAuth(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return chainAPI(mwBasicAuth(h, "bench", "bench"))
}
func chainSecurity(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return chainAuth(mwCSRFSkip(mwSecure(h)))
}
func chainFullstack(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return chainSecurity(mwRateLimit(mwTimeoutDummy(mwBodyLimit(h, 10<<20))))
}

// mwRequestID echoes X-Request-Id or generates a new UUID.
func mwRequestID(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		id := string(rc.Request.Header.Peek("X-Request-Id"))
		if id == "" {
			id = uuid.NewString()
		}
		rc.Response.Header.Set("X-Request-Id", id)
		next(rc)
	}
}

// mwLoggerDiscard simulates a logger formatting a one-liner and writing
// to io.Discard so the bench sees the formatter overhead but no stderr.
func mwLoggerDiscard(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		_, _ = io.WriteString(io.Discard, string(rc.Method())+" "+string(rc.Path())+"\n")
		next(rc)
	}
}

// mwRecovery catches panics from next.
func mwRecovery(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		defer func() {
			if rec := recover(); rec != nil {
				rc.SetStatusCode(fasthttp.StatusInternalServerError)
				_, _ = rc.WriteString("internal error")
			}
		}()
		next(rc)
	}
}

// mwCORS sets allow-all CORS headers and short-circuits preflight.
func mwCORS(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		rc.Response.Header.Set("Access-Control-Allow-Origin", "*")
		rc.Response.Header.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		rc.Response.Header.Set("Access-Control-Allow-Headers", "*")
		if string(rc.Method()) == http.MethodOptions &&
			len(rc.Request.Header.Peek("Access-Control-Request-Method")) > 0 {
			rc.SetStatusCode(fasthttp.StatusNoContent)
			return
		}
		next(rc)
	}
}

// mwBasicAuth enforces user:pass on Authorization via constant-time
// compare. Matches the wire behavior of the other perfmatrix stacks.
func mwBasicAuth(next fasthttp.RequestHandler, user, pass string) fasthttp.RequestHandler {
	expect := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	expectBytes := []byte(expect)
	return func(rc *fasthttp.RequestCtx) {
		auth := string(rc.Request.Header.Peek("Authorization"))
		if !strings.HasPrefix(auth, "Basic ") {
			rc.Response.Header.Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			rc.SetStatusCode(fasthttp.StatusUnauthorized)
			_, _ = rc.WriteString("unauthorized")
			return
		}
		got := []byte(auth[len("Basic "):])
		if subtle.ConstantTimeCompare(got, expectBytes) != 1 {
			rc.Response.Header.Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			rc.SetStatusCode(fasthttp.StatusUnauthorized)
			_, _ = rc.WriteString("unauthorized")
			return
		}
		next(rc)
	}
}

// mwCSRFSkip emits a CSRF cookie without validation; see godoc on
// stdhttp/chain_handlers.go for the rationale.
func mwCSRFSkip(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		ck := fasthttp.AcquireCookie()
		ck.SetKey("_csrf")
		ck.SetValue("skip-token-bench")
		ck.SetPath("/")
		ck.SetHTTPOnly(true)
		rc.Response.Header.SetCookie(ck)
		fasthttp.ReleaseCookie(ck)
		next(rc)
	}
}

// mwSecure emits OWASP security headers.
func mwSecure(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		h := &rc.Response.Header
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-XSS-Protection", "0")
		next(rc)
	}
}

var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func mwRateLimit(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		if !chainLimiter.Allow() {
			rc.SetStatusCode(fasthttp.StatusTooManyRequests)
			_, _ = rc.WriteString("rate limited")
			return
		}
		next(rc)
	}
}

// timeoutCounter records the number of timed-out responses. Used to
// keep the linter happy on the no-op timeout middleware; not read.
var timeoutCounter atomic.Int64

// mwTimeoutDummy approximates a timeout wrapper. fasthttp sets request
// deadlines on its underlying transport, so a per-request context
// deadline has little effect — the decorator runs a branch and defers
// a cleanup, which matches the observable cost in the other packages.
func mwTimeoutDummy(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		defer timeoutCounter.Add(0)
		next(rc)
	}
}

// mwBodyLimit rejects requests whose body exceeds maxBytes. fasthttp
// reads the body into rc.PostBody() so we only need a length check.
func mwBodyLimit(next fasthttp.RequestHandler, maxBytes int) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		if len(rc.Request.Body()) > maxBytes {
			rc.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
			_, _ = rc.WriteString("body too large")
			return
		}
		next(rc)
	}
}
