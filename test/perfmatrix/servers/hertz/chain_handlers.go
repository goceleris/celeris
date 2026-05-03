package hertz

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// mountChainHandlers mounts the 4 middleware chains on the hertz engine
// as native app.HandlerFunc decorators. Hertz has hertz-contrib/*
// packages for most middlewares but we hand-roll here so the overhead
// shape mirrors every other perfmatrix competitor's decorator stack.
func mountChainHandlers(s *Server) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ensureEngineLocked(nil)
	if s.mountedChain {
		s.mu.Unlock()
		return
	}
	s.mountedChain = true
	s.mu.Unlock()

	jsonTerminal := func(_ context.Context, rc *app.RequestContext) {
		rc.SetContentType("application/json")
		rc.SetStatusCode(http.StatusOK)
		_, _ = rc.Write(s.jsonSmall)
	}
	uploadTerminal := func(_ context.Context, rc *app.RequestContext) {
		_ = rc.Request.Body()
		rc.SetContentType("text/plain; charset=utf-8")
		rc.SetStatusCode(http.StatusOK)
		_, _ = rc.Write([]byte("OK"))
	}

	for _, spec := range []struct {
		prefix string
		mws    []app.HandlerFunc
	}{
		{"/chain/api/", chainAPI()},
		{"/chain/auth/", chainAuth()},
		{"/chain/security/", chainSecurity()},
		{"/chain/fullstack/", chainFullstack()},
	} {
		jsonHandlers := append([]app.HandlerFunc{}, spec.mws...)
		jsonHandlers = append(jsonHandlers, jsonTerminal)
		uploadHandlers := append([]app.HandlerFunc{}, spec.mws...)
		uploadHandlers = append(uploadHandlers, uploadTerminal)
		s.h.GET(spec.prefix+"json", jsonHandlers...)
		s.h.POST(spec.prefix+"upload", uploadHandlers...)
	}
}

func chainAPI() []app.HandlerFunc {
	return []app.HandlerFunc{hertzRequestID, hertzLoggerDiscard, hertzRecovery, hertzCORS}
}
func chainAuth() []app.HandlerFunc {
	return append(chainAPI(), hertzBasicAuth("bench", "bench"))
}
func chainSecurity() []app.HandlerFunc {
	return append(chainAuth(), hertzCSRFSkip, hertzSecure)
}
func chainFullstack() []app.HandlerFunc {
	return append(chainSecurity(), hertzRateLimit, hertzTimeoutDummy, hertzBodyLimit(10<<20))
}

func hertzRequestID(_ context.Context, rc *app.RequestContext) {
	id := string(rc.GetHeader("X-Request-Id"))
	if id == "" {
		id = uuid.NewString()
	}
	rc.Response.Header.Set("X-Request-Id", id)
	rc.Next(context.Background())
}

func hertzLoggerDiscard(_ context.Context, rc *app.RequestContext) {
	_, _ = io.WriteString(io.Discard, string(rc.Method())+" "+string(rc.Path())+"\n")
	rc.Next(context.Background())
}

func hertzRecovery(ctx context.Context, rc *app.RequestContext) {
	defer func() {
		if rec := recover(); rec != nil {
			rc.SetStatusCode(http.StatusInternalServerError)
			_, _ = rc.Write([]byte("internal error"))
		}
	}()
	rc.Next(ctx)
}

func hertzCORS(ctx context.Context, rc *app.RequestContext) {
	rc.Response.Header.Set("Access-Control-Allow-Origin", "*")
	rc.Response.Header.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
	rc.Response.Header.Set("Access-Control-Allow-Headers", "*")
	if string(rc.Method()) == http.MethodOptions &&
		len(rc.GetHeader("Access-Control-Request-Method")) > 0 {
		rc.SetStatusCode(http.StatusNoContent)
		rc.Abort()
		return
	}
	rc.Next(ctx)
}

func hertzBasicAuth(user, pass string) app.HandlerFunc {
	expect := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	expectBytes := []byte(expect)
	return func(ctx context.Context, rc *app.RequestContext) {
		auth := string(rc.GetHeader("Authorization"))
		if !strings.HasPrefix(auth, "Basic ") {
			rc.Response.Header.Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			rc.SetStatusCode(http.StatusUnauthorized)
			_, _ = rc.Write([]byte("unauthorized"))
			rc.Abort()
			return
		}
		got := []byte(auth[len("Basic "):])
		if subtle.ConstantTimeCompare(got, expectBytes) != 1 {
			rc.Response.Header.Set("WWW-Authenticate", `Basic realm="perfmatrix"`)
			rc.SetStatusCode(http.StatusUnauthorized)
			_, _ = rc.Write([]byte("unauthorized"))
			rc.Abort()
			return
		}
		rc.Next(ctx)
	}
}

func hertzCSRFSkip(ctx context.Context, rc *app.RequestContext) {
	ck := &protocol.Cookie{}
	ck.SetKey("_csrf")
	ck.SetValue("skip-token-bench")
	ck.SetPath("/")
	ck.SetHTTPOnly(true)
	rc.Response.Header.SetCookie(ck)
	rc.Next(ctx)
}

func hertzSecure(ctx context.Context, rc *app.RequestContext) {
	h := &rc.Response.Header
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-XSS-Protection", "0")
	rc.Next(ctx)
}

var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func hertzRateLimit(ctx context.Context, rc *app.RequestContext) {
	if !chainLimiter.Allow() {
		rc.SetStatusCode(http.StatusTooManyRequests)
		_, _ = rc.Write([]byte("rate limited"))
		rc.Abort()
		return
	}
	rc.Next(ctx)
}

// hertzTimeoutDummy approximates a timeout middleware. hertz propagates
// context.Context through Next(ctx), so we attach a deadline that
// downstream handlers can honour; the overhead is a defer cancel().
func hertzTimeoutDummy(ctx context.Context, rc *app.RequestContext) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc.Next(cctx)
}

func hertzBodyLimit(limit int) app.HandlerFunc {
	return func(ctx context.Context, rc *app.RequestContext) {
		if len(rc.Request.Body()) > limit {
			rc.SetStatusCode(http.StatusRequestEntityTooLarge)
			_, _ = rc.Write([]byte("body too large"))
			rc.Abort()
			return
		}
		rc.Next(ctx)
	}
}
