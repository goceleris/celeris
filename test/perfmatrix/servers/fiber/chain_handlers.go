package fiber

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	fiberv3 "github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// mountChainHandlers mounts the 4 middleware chains on the fiber app
// using native fiberv3 middleware handlers. Fiber ships a full suite
// (fiber/v3/middleware/requestid, recover, cors, basicauth, ...) but
// we hand-roll each middleware here to keep the observable overhead
// shape identical to every other perfmatrix server — the bench
// measures uniform decorator cost across frameworks, not each
// ecosystem's idiomatic style.
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

	jsonTerminal := func(c fiberv3.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(s.jsonSmall)
	}
	uploadTerminal := func(c fiberv3.Ctx) error {
		_ = c.Body()
		c.Set("Content-Type", "text/plain; charset=utf-8")
		return c.SendString("OK")
	}

	for _, spec := range []struct {
		prefix   string
		wrappers []fiberv3.Handler
	}{
		{"/chain/api", chainAPI()},
		{"/chain/auth", chainAuth()},
		{"/chain/security", chainSecurity()},
		{"/chain/fullstack", chainFullstack()},
	} {
		prefix := spec.prefix
		for _, mw := range spec.wrappers {
			s.app.Use(prefix, mw)
		}
		s.app.Get(prefix+"/json", jsonTerminal)
		s.app.Post(prefix+"/upload", uploadTerminal)
	}
}

// Each chainX() returns the list of fiber middleware handlers to mount
// in order in front of the terminal handler. The ordering matches the
// other perfmatrix competitor packages.
func chainAPI() []fiberv3.Handler {
	return []fiberv3.Handler{fiberRequestID, fiberLoggerDiscard, fiberRecovery, fiberCORS}
}

func chainAuth() []fiberv3.Handler {
	return append(chainAPI(), fiberBasicAuth("bench", "bench"))
}

func chainSecurity() []fiberv3.Handler {
	return append(chainAuth(), fiberCSRFSkip, fiberSecure)
}

func chainFullstack() []fiberv3.Handler {
	return append(chainSecurity(), fiberRateLimit, fiberTimeoutDummy, fiberBodyLimit(10<<20))
}

// fiberRequestID assigns X-Request-Id from the incoming header or a new
// UUID and mirrors it onto the response.
func fiberRequestID(c fiberv3.Ctx) error {
	id := c.Get("X-Request-Id")
	if id == "" {
		id = uuid.NewString()
	}
	c.Set("X-Request-Id", id)
	return c.Next()
}

// fiberLoggerDiscard writes a one-liner to io.Discard so the logger's
// formatting cost shows up in the bench without polluting stderr.
func fiberLoggerDiscard(c fiberv3.Ctx) error {
	_, _ = io.WriteString(io.Discard, c.Method()+" "+c.Path()+"\n")
	return c.Next()
}

// fiberRecovery defers recover() on the downstream handler.
func fiberRecovery(c fiberv3.Ctx) error {
	defer func() {
		if rec := recover(); rec != nil {
			c.Status(http.StatusInternalServerError)
			_ = c.SendString("internal error")
		}
	}()
	return c.Next()
}

// fiberCORS sets the allow-all CORS headers and short-circuits preflight.
func fiberCORS(c fiberv3.Ctx) error {
	c.Set("Access-Control-Allow-Origin", "*")
	c.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
	c.Set("Access-Control-Allow-Headers", "*")
	if c.Method() == http.MethodOptions && c.Get("Access-Control-Request-Method") != "" {
		return c.SendStatus(http.StatusNoContent)
	}
	return c.Next()
}

// fiberBasicAuth returns a handler enforcing user:pass on Authorization.
func fiberBasicAuth(user, pass string) fiberv3.Handler {
	expect := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	expectBytes := []byte(expect)
	return func(c fiberv3.Ctx) error {
		auth := c.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") {
			c.Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			c.Status(http.StatusUnauthorized)
			return c.SendString("unauthorized")
		}
		got := []byte(auth[len("Basic "):])
		if subtle.ConstantTimeCompare(got, expectBytes) != 1 {
			c.Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			c.Status(http.StatusUnauthorized)
			return c.SendString("unauthorized")
		}
		return c.Next()
	}
}

// fiberCSRFSkip emits a CSRF cookie on every response but skips
// validation. See godoc on stdhttp's mwCSRFSkip for the rationale
// (loadgen cannot fake CSRF tokens).
func fiberCSRFSkip(c fiberv3.Ctx) error {
	c.Cookie(&fiberv3.Cookie{Name: "_csrf", Value: "skip-token-bench", Path: "/", HTTPOnly: true})
	return c.Next()
}

// fiberSecure emits OWASP security headers (mirrors unrolled/secure's
// default set).
func fiberSecure(c fiberv3.Ctx) error {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("X-Frame-Options", "SAMEORIGIN")
	c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	c.Set("X-XSS-Protection", "0")
	return c.Next()
}

var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func fiberRateLimit(c fiberv3.Ctx) error {
	if !chainLimiter.Allow() {
		c.Status(http.StatusTooManyRequests)
		return c.SendString("rate limited")
	}
	return c.Next()
}

// fiberTimeoutDummy approximates a timeout middleware. Fiber's Context
// does not surface a context.CancelFunc directly; for this bench the
// observable cost is a method call + branch, which matches the
// competitor implementations.
func fiberTimeoutDummy(c fiberv3.Ctx) error {
	_ = 30 * time.Second
	return c.Next()
}

func fiberBodyLimit(limit int) fiberv3.Handler {
	return func(c fiberv3.Ctx) error {
		if len(c.Body()) > limit {
			c.Status(http.StatusRequestEntityTooLarge)
			return c.SendString("body too large")
		}
		return c.Next()
	}
}
