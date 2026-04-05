package logger

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

func TestLoggerDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	out := buf.String()
	if !strings.Contains(out, `"method":"GET"`) {
		t.Fatalf("missing method in log: %s", out)
	}
	if !strings.Contains(out, `"path":"/"`) {
		t.Fatalf("missing path in log: %s", out)
	}
	if !strings.Contains(out, `"status":200`) {
		t.Fatalf("missing status in log: %s", out)
	}
}

func TestLoggerSkipPaths(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SkipPaths: []string{"/health"}})
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/health")
	testutil.AssertNoError(t, err)
	if buf.Len() > 0 {
		t.Fatalf("expected no log for skipped path, got: %s", buf.String())
	}
}

func TestLoggerSkipFunc(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output: log,
		Skip: func(c *celeris.Context) bool {
			return c.Path() == "/skip"
		},
	})
	_, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/skip")
	testutil.AssertNoError(t, err)
	if buf.Len() > 0 {
		t.Fatalf("expected no log for skipped request, got: %s", buf.String())
	}
}

func TestLoggerErrorLevel(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(500, "error")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/fail")
	testutil.AssertNoError(t, err)
	if !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Fatalf("expected ERROR level for 500, got: %s", buf.String())
	}
}

func TestLoggerWarnLevel(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(404, "not found")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/missing")
	testutil.AssertNoError(t, err)
	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Fatalf("expected WARN level for 404, got: %s", buf.String())
	}
}

func TestLoggerRequestID(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-request-id", "abc-123"))
	testutil.AssertNoError(t, err)
	if !strings.Contains(buf.String(), `"request_id":"abc-123"`) {
		t.Fatalf("missing request_id in log: %s", buf.String())
	}
}

func TestLoggerCustomFields(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output: log,
		Fields: func(_ *celeris.Context, _ time.Duration) []slog.Attr {
			return []slog.Attr{slog.String("custom", "value")}
		},
	})
	_, err := testutil.RunMiddleware(t, mw)
	testutil.AssertNoError(t, err)
	if !strings.Contains(buf.String(), `"custom":"value"`) {
		t.Fatalf("missing custom field in log: %s", buf.String())
	}
}

func TestLoggerClientIP(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	_, err := testutil.RunMiddleware(t, mw, celeristest.WithHeader("x-forwarded-for", "1.2.3.4"))
	testutil.AssertNoError(t, err)
	if !strings.Contains(buf.String(), `"client_ip":"1.2.3.4"`) {
		t.Fatalf("missing client_ip in log: %s", buf.String())
	}
}

func TestLoggerHandlerError(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(503, "service unavailable")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/err")
	testutil.AssertError(t, err)
	if !strings.Contains(buf.String(), `"error"`) {
		t.Fatalf("missing error field in log: %s", buf.String())
	}
}

func FuzzLoggerSkipPaths(f *testing.F) {
	f.Add("/health")
	f.Add("/metrics")
	f.Add("/")
	f.Add("")
	f.Fuzz(func(t *testing.T, path string) {
		log, buf := newTestLogger()
		mw := New(Config{Output: log, SkipPaths: []string{path}})
		ctx, _ := celeristest.NewContextT(t, "GET", path)
		_ = mw(ctx)
		if buf.Len() > 0 {
			t.Errorf("path %q should be skipped but was logged", path)
		}
	})
}

func FuzzLoggerStatus(f *testing.F) {
	f.Add(200)
	f.Add(404)
	f.Add(500)
	f.Add(0)
	f.Add(-1)
	f.Fuzz(func(_ *testing.T, status int) {
		_ = defaultLevel(status)
	})
}

func TestLoggerCaptureRequestBody(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Output: log, CaptureRequestBody: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "POST", "/body",
		celeristest.WithBody([]byte("hello world")))
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("expected request body in log: %s", buf.String())
	}
}

func TestLoggerCaptureResponseBody(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Output: log, CaptureResponseBody: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "response data")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/resp")
	if !strings.Contains(buf.String(), "response data") {
		t.Fatalf("expected response body in log: %s", buf.String())
	}
}

func TestLoggerMaxCaptureBytes(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Output: log, CaptureRequestBody: true, MaxCaptureBytes: 5})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "POST", "/trunc",
		celeristest.WithBody([]byte("hello world long body")))
	output := buf.String()
	if !strings.Contains(output, "hello") {
		t.Fatalf("expected truncated body in log: %s", output)
	}
	if strings.Contains(output, "hello world") {
		t.Fatalf("body should be truncated to 5 bytes: %s", output)
	}
}

func TestFastHandlerColorOutput(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	log := slog.New(h)
	log.Info("test message")
	output := buf.String()
	if !strings.Contains(output, "\033[32m") {
		t.Fatalf("expected green color code for INFO, got: %q", output)
	}
	if !strings.Contains(output, "\033[0m") {
		t.Fatalf("expected color reset, got: %q", output)
	}
	if !strings.Contains(output, "INFO") {
		t.Fatalf("expected INFO in output, got: %q", output)
	}
}

func TestFastHandlerColorError(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	log := slog.New(h)
	log.Error("error message")
	output := buf.String()
	if !strings.Contains(output, "\033[31m") {
		t.Fatalf("expected red color code for ERROR, got: %q", output)
	}
}

func TestFastHandlerColorWarn(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	log := slog.New(h)
	log.Warn("warn message")
	output := buf.String()
	if !strings.Contains(output, "\033[33m") {
		t.Fatalf("expected yellow color code for WARN, got: %q", output)
	}
}

func TestFastHandlerColorDebug(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Level: slog.LevelDebug, Color: true})
	log := slog.New(h)
	log.Debug("debug message")
	output := buf.String()
	if !strings.Contains(output, "\033[36m") {
		t.Fatalf("expected cyan color code for DEBUG, got: %q", output)
	}
}

func TestFastHandlerNoColorByDefault(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, nil)
	log := slog.New(h)
	log.Info("test")
	output := buf.String()
	if strings.Contains(output, "\033[") {
		t.Fatalf("expected no color codes with default options, got: %q", output)
	}
}

func TestFastHandlerColorDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: false})
	log := slog.New(h)
	log.Info("test")
	output := buf.String()
	if strings.Contains(output, "\033[") {
		t.Fatalf("expected no color codes with Color=false, got: %q", output)
	}
}

func TestFastHandlerColorWithAttrs(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	log := slog.New(h).With("key", "value")
	log.Info("attrs test")
	output := buf.String()
	if !strings.Contains(output, "\033[32m") {
		t.Fatalf("expected color preserved through WithAttrs, got: %q", output)
	}
}

func TestFastHandlerColorWithGroup(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	log := slog.New(h).WithGroup("grp")
	log.Info("group test", "k", "v")
	output := buf.String()
	if !strings.Contains(output, "\033[32m") {
		t.Fatalf("expected color preserved through WithGroup, got: %q", output)
	}
}

func TestFastHandlerColorStatus(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	r.AddAttrs(slog.Int("status", 200))
	_ = h.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "\033[32m") { // green for 2xx
		t.Fatalf("expected green for 200 status, got: %q", output)
	}
}

func TestFastHandlerColorStatus500(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	r := slog.NewRecord(time.Now(), slog.LevelError, "request", 0)
	r.AddAttrs(slog.Int("status", 503))
	_ = h.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "\033[31m503") {
		t.Fatalf("expected red for 503 status, got: %q", output)
	}
}

func TestFastHandlerColorMethod(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	r.AddAttrs(slog.String("method", "GET"))
	_ = h.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "\033[34m") { // blue for GET
		t.Fatalf("expected blue for GET method, got: %q", output)
	}
}

func TestFastHandlerColorLatency(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: true})
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	r.AddAttrs(slog.Duration("latency", 500*time.Microsecond))
	_ = h.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "\033[32m") { // green for <1ms
		t.Fatalf("expected green for sub-ms latency, got: %q", output)
	}
}

func TestAttrPoolResetOnHandlerError(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(503, "service unavailable")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	// Run three times to exercise the attrPool reuse path after an error.
	for i := range 3 {
		buf.Reset()
		_, err := testutil.RunChain(t, chain, "GET", "/err")
		testutil.AssertError(t, err)
		out := buf.String()
		if !strings.Contains(out, `"error"`) {
			t.Fatalf("iter %d: missing error field in log: %s", i, out)
		}
		// Verify no stale fields leak from a previous iteration by
		// ensuring the log entry does not contain duplicate method fields.
		count := strings.Count(out, `"method"`)
		if count != 1 {
			t.Fatalf("iter %d: expected 1 method field, got %d: %s", i, count, out)
		}
	}
}

func TestFastHandlerNoColorOnValues(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{Color: false})
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	r.AddAttrs(slog.Int("status", 200), slog.String("method", "GET"))
	_ = h.Handle(context.Background(), r)
	output := buf.String()
	if strings.Contains(output, "\033[") {
		t.Fatalf("expected no color codes with Color=false, got: %q", output)
	}
}

func TestSensitiveHeadersRedacted(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{"Authorization", "X-Api-Key"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/secret",
		celeristest.WithHeader("authorization", "Bearer token123"),
		celeristest.WithHeader("x-api-key", "secret-key"),
		celeristest.WithHeader("content-type", "application/json"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in log for sensitive headers, got: %s", out)
	}
	if strings.Contains(out, "token123") {
		t.Fatalf("authorization value should be redacted, got: %s", out)
	}
	if strings.Contains(out, "secret-key") {
		t.Fatalf("x-api-key value should be redacted, got: %s", out)
	}
}

func TestSensitiveHeadersCaseInsensitive(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{"AUTHORIZATION"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/case",
		celeristest.WithHeader("authorization", "Bearer xyz"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected case-insensitive redaction, got: %s", out)
	}
}

func TestNoSensitiveHeadersNoRedaction(t *testing.T) {
	log, buf := newTestLogger()
	// Explicitly set empty slice to disable all redaction.
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-redact",
		celeristest.WithHeader("authorization", "Bearer visible"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected no redaction with empty SensitiveHeaders, got: %s", out)
	}
}

func TestLogHost(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogHost: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/host")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"host":"localhost"`) {
		t.Fatalf("expected host field in log, got: %s", out)
	}
}

func TestLogHostDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-host")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"host"`) {
		t.Fatalf("host should not be logged by default, got: %s", out)
	}
}

func TestLogUserAgent(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogUserAgent: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/ua",
		celeristest.WithHeader("user-agent", "TestAgent/1.0"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"user_agent":"TestAgent/1.0"`) {
		t.Fatalf("expected user_agent field in log, got: %s", out)
	}
}

func TestLogUserAgentDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-ua",
		celeristest.WithHeader("user-agent", "TestAgent/1.0"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"user_agent"`) {
		t.Fatalf("user_agent should not be logged by default, got: %s", out)
	}
}

func TestLogReferer(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogReferer: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/ref",
		celeristest.WithHeader("referer", "https://example.com/page"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"referer":"https://example.com/page"`) {
		t.Fatalf("expected referer field in log, got: %s", out)
	}
}

func TestLogRefererDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-ref",
		celeristest.WithHeader("referer", "https://example.com"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"referer"`) {
		t.Fatalf("referer should not be logged by default, got: %s", out)
	}
}

func TestLogRouteDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/users/42")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"route"`) {
		t.Fatalf("route should not be logged by default, got: %s", out)
	}
}

func TestLogRouteEmptyFullPath(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogRoute: true})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-route")
	testutil.AssertNoError(t, err)
	out := buf.String()
	// FullPath is only set by the router; test contexts have it empty,
	// so the field should be omitted.
	if strings.Contains(out, `"route"`) {
		t.Fatalf("route should be omitted when FullPath empty, got: %s", out)
	}
}

func TestAppendDurationNegative(t *testing.T) {
	buf := appendDuration(nil, -500*time.Microsecond)
	got := string(buf)
	want := (-500 * time.Microsecond).String()
	if got != want {
		t.Fatalf("appendDuration(-500µs) = %q, want %q", got, want)
	}
}

func TestAppendDurationNegativeMs(t *testing.T) {
	buf := appendDuration(nil, -42*time.Millisecond)
	got := string(buf)
	want := (-42 * time.Millisecond).String()
	if got != want {
		t.Fatalf("appendDuration(-42ms) = %q, want %q", got, want)
	}
}

func TestAppendDurationNegativeSeconds(t *testing.T) {
	buf := appendDuration(nil, -3*time.Second)
	got := string(buf)
	want := (-3 * time.Second).String()
	if got != want {
		t.Fatalf("appendDuration(-3s) = %q, want %q", got, want)
	}
}

func TestAllOptInFieldsTogether(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:       log,
		LogHost:      true,
		LogUserAgent: true,
		LogReferer:   true,
		LogScheme:    true,
		LogRoute:     true,
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/all",
		celeristest.WithHeader("user-agent", "AllTest/1.0"),
		celeristest.WithHeader("referer", "https://all.example.com"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	for _, field := range []string{`"host"`, `"user_agent"`, `"referer"`} {
		if !strings.Contains(out, field) {
			t.Fatalf("missing %s in log with all opt-in fields: %s", field, out)
		}
	}
}

// --- TimeZone ---

func TestTimeZone(t *testing.T) {
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(h)

	utc := time.UTC
	mw := New(Config{
		Output:           log,
		TimeZone:         utc,
		SensitiveHeaders: []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/tz")
	testutil.AssertNoError(t, err)
	out := buf.String()
	// The TextHandler formats the record's time. Since we set UTC,
	// the timestamp must end with Z (UTC indicator).
	if !strings.Contains(out, "Z") {
		t.Fatalf("expected UTC timestamp (Z suffix) in log, got: %s", out)
	}
}

func TestTimeZoneNilUsesLocal(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/tz-nil")
	testutil.AssertNoError(t, err)
	if buf.Len() == 0 {
		t.Fatal("expected log output")
	}
}

// --- TimeFormat on FastHandler ---

func TestFastHandlerTimeFormat(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{TimeFormat: time.Kitchen})
	r := slog.NewRecord(time.Date(2025, 6, 15, 14, 30, 0, 0, time.UTC), slog.LevelInfo, "test", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// time.Kitchen produces e.g. "2:30PM"
	if !strings.Contains(out, "time=2:30PM") {
		t.Fatalf("expected Kitchen format in output, got: %q", out)
	}
}

func TestFastHandlerTimeFormatEmpty(t *testing.T) {
	buf := &bytes.Buffer{}
	h := NewFastHandler(buf, &FastHandlerOptions{TimeFormat: ""})
	now := time.Date(2025, 6, 15, 14, 30, 45, 123_000_000, time.UTC)
	r := slog.NewRecord(now, slog.LevelInfo, "test", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Default RFC3339-millis format
	if !strings.Contains(out, "2025-06-15T14:30:45.123Z") {
		t.Fatalf("expected default RFC3339 format, got: %q", out)
	}
}

// --- LogPID ---

func TestLogPID(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogPID: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/pid")
	testutil.AssertNoError(t, err)
	out := buf.String()
	pid := strconv.Itoa(os.Getpid())
	if !strings.Contains(out, `"pid":`+pid) {
		t.Fatalf("expected pid=%s in log, got: %s", pid, out)
	}
}

func TestLogPIDDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-pid")
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"pid"`) {
		t.Fatalf("pid should not be logged by default, got: %s", buf.String())
	}
}

func TestLogPIDCached(t *testing.T) {
	// Verify cachedPID matches os.Getpid().
	if cachedPID != os.Getpid() {
		t.Fatalf("cachedPID=%d, os.Getpid()=%d", cachedPID, os.Getpid())
	}
}

// --- LogQueryParams ---

func TestLogQueryParams(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogQueryParams: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/search",
		celeristest.WithQuery("q", "hello"),
		celeristest.WithQuery("page", "1"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"query"`) {
		t.Fatalf("expected query attr in log, got: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected query value 'hello' in log, got: %s", out)
	}
}

func TestLogQueryParamsEmpty(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogQueryParams: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-query")
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"query"`) {
		t.Fatalf("query should be omitted when empty, got: %s", buf.String())
	}
}

func TestLogQueryParamsDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/q",
		celeristest.WithQuery("q", "test"),
	)
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"query"`) {
		t.Fatalf("query should not be logged by default, got: %s", buf.String())
	}
}

// --- LogFormValues ---

func TestLogFormValues(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogFormValues: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("user=alice&role=admin")),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"form"`) {
		t.Fatalf("expected form attr in log, got: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("expected form value 'alice' in log, got: %s", out)
	}
}

func TestLogFormValuesNotFormContentType(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogFormValues: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/json",
		celeristest.WithHeader("content-type", "application/json"),
		celeristest.WithBody([]byte(`{"key":"value"}`)),
	)
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"form"`) {
		t.Fatalf("form should not be logged for JSON content-type, got: %s", buf.String())
	}
}

func TestLogFormValuesDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-off",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("x=1")),
	)
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"form"`) {
		t.Fatalf("form should not be logged by default, got: %s", buf.String())
	}
}

// --- LogCookies ---

func TestLogCookies(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogCookies: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/cookies",
		celeristest.WithCookie("session", "abc123"),
		celeristest.WithCookie("theme", "dark"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"cookies"`) {
		t.Fatalf("expected cookies attr in log, got: %s", out)
	}
	if !strings.Contains(out, "session") {
		t.Fatalf("expected cookie name 'session' in log, got: %s", out)
	}
	if !strings.Contains(out, "theme") {
		t.Fatalf("expected cookie name 'theme' in log, got: %s", out)
	}
	// Values must NOT appear in the log.
	if strings.Contains(out, "abc123") {
		t.Fatalf("cookie value 'abc123' should not be in log, got: %s", out)
	}
	if strings.Contains(out, "dark") {
		// "dark" could appear as part of a cookie name coincidentally,
		// but in the "cookies" attr it should only be the name "theme".
		// Check that the cookies field does not contain "=dark".
		if strings.Contains(out, "=dark") {
			t.Fatalf("cookie value should not be in log, got: %s", out)
		}
	}
}

func TestLogCookiesNoCookies(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogCookies: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-cookies")
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"cookies"`) {
		t.Fatalf("cookies should be omitted when no cookies present, got: %s", buf.String())
	}
}

func TestLogCookiesDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/cookies-off",
		celeristest.WithCookie("sess", "val"),
	)
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"cookies"`) {
		t.Fatalf("cookies should not be logged by default, got: %s", buf.String())
	}
}

// --- Done callback ---

func TestDoneCallback(t *testing.T) {
	log, _ := newTestLogger()
	var mu sync.Mutex
	var called bool
	var gotStatus int
	var gotLatency time.Duration

	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{},
		Done: func(_ *celeris.Context, latency time.Duration, status int) {
			mu.Lock()
			called = true
			gotStatus = status
			gotLatency = latency
			mu.Unlock()
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/done")
	testutil.AssertNoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("Done callback was not called")
	}
	if gotStatus != 200 {
		t.Fatalf("Done got status=%d, want 200", gotStatus)
	}
	if gotLatency <= 0 {
		t.Fatalf("Done got latency=%v, want >0", gotLatency)
	}
}

func TestDoneCallbackOn5xx(t *testing.T) {
	log, _ := newTestLogger()
	var mu sync.Mutex
	var gotStatus int

	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{},
		Done: func(_ *celeris.Context, _ time.Duration, status int) {
			mu.Lock()
			gotStatus = status
			mu.Unlock()
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(503, "error")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/done-5xx")
	testutil.AssertNoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	if gotStatus != 503 {
		t.Fatalf("Done got status=%d, want 503", gotStatus)
	}
}

func TestDoneCallbackNilSafe(t *testing.T) {
	log, _ := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/done-nil")
	testutil.AssertNoError(t, err)
	// No panic means success.
}

func TestDoneCallbackCalledWhenLogDisabled(t *testing.T) {
	// Use a handler that only enables ERROR level.
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})
	log := slog.New(h)

	var mu sync.Mutex
	var called bool

	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{},
		Done: func(_ *celeris.Context, _ time.Duration, _ int) {
			mu.Lock()
			called = true
			mu.Unlock()
		},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok") // INFO level → disabled
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/done-disabled")
	testutil.AssertNoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("Done callback should be called even when log level is disabled")
	}
}

// --- DefaultSensitiveHeaders ---

func TestDefaultSensitiveHeadersApplied(t *testing.T) {
	log, buf := newTestLogger()
	// SensitiveHeaders is nil → use DefaultSensitiveHeaders.
	mw := New(Config{Output: log})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/defaults",
		celeristest.WithHeader("authorization", "Bearer secret"),
		celeristest.WithHeader("x-api-key", "key123"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected default sensitive headers to be redacted, got: %s", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("authorization value should be redacted, got: %s", out)
	}
	if strings.Contains(out, "key123") {
		t.Fatalf("x-api-key value should be redacted, got: %s", out)
	}
}

func TestDefaultSensitiveHeadersOverridden(t *testing.T) {
	log, buf := newTestLogger()
	// Explicitly set only "x-custom" → defaults NOT used.
	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{"x-custom"},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/override",
		celeristest.WithHeader("authorization", "Bearer visible"),
		celeristest.WithHeader("x-custom", "secret"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	// authorization should NOT be redacted (not in custom list).
	if strings.Contains(out, "header.authorization") {
		t.Fatalf("authorization should not be redacted with custom list, got: %s", out)
	}
	// x-custom SHOULD be redacted.
	if !strings.Contains(out, "header.x-custom") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("x-custom should be redacted, got: %s", out)
	}
}

func TestDefaultSensitiveHeadersDisabled(t *testing.T) {
	log, buf := newTestLogger()
	// Empty slice disables all redaction.
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/disabled",
		celeristest.WithHeader("authorization", "Bearer visible"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected no redaction with empty SensitiveHeaders, got: %s", out)
	}
}

func TestDefaultSensitiveHeadersFunc(t *testing.T) {
	// Verify the function returns the expected entries.
	expected := map[string]bool{
		"authorization": true,
		"cookie":        true,
		"set-cookie":    true,
		"x-api-key":     true,
	}
	got := DefaultSensitiveHeaders()
	if len(got) != len(expected) {
		t.Fatalf("DefaultSensitiveHeaders() has %d entries, want %d",
			len(got), len(expected))
	}
	for _, h := range got {
		if !expected[h] {
			t.Fatalf("unexpected header in DefaultSensitiveHeaders(): %s", h)
		}
	}
	// Verify mutation safety: modifying the returned slice does not
	// affect subsequent calls.
	got[0] = "MUTATED"
	got2 := DefaultSensitiveHeaders()
	for _, h := range got2 {
		if h == "MUTATED" {
			t.Fatal("DefaultSensitiveHeaders() returned a shared slice — mutation leaked")
		}
	}
}

// --- parseCookieNames ---

func TestParseCookieNames(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"single", "session=abc", "session"},
		{"multiple", "session=abc; theme=dark", "session,theme"},
		{"trailing semicolon", "a=1; b=2;", "a,b"},
		{"no value", "bare", "bare"},
		{"empty", "", ""},
		{"spaces", "  a=1 ;  b=2  ", "a,b"},
		{"equals in value", "data=a=b; other=c", "data,other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCookieNames(tc.raw)
			if got != tc.want {
				t.Errorf("parseCookieNames(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// --- Pool capacity ---

func TestAttrPoolCapacity(t *testing.T) {
	ptr := attrPool.Get().(*[]slog.Attr)
	got := cap(*ptr)
	attrPool.Put(ptr)
	if got < 32 {
		t.Fatalf("attrPool capacity = %d, want >= 32", got)
	}
}

// --- LogBytesIn ---

func TestLogBytesIn(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogBytesIn: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/bytes-in",
		celeristest.WithBody([]byte("hello world")),
		celeristest.WithHeader("content-length", "11"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"bytes_in":11`) {
		t.Fatalf("expected bytes_in=11 in log, got: %s", out)
	}
}

func TestLogBytesInDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/no-bytes-in",
		celeristest.WithBody([]byte("data")),
		celeristest.WithHeader("content-length", "4"),
	)
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"bytes_in"`) {
		t.Fatalf("bytes_in should not be logged by default, got: %s", buf.String())
	}
}

// --- LogScheme ---

func TestLogScheme(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, LogScheme: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/scheme")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"scheme"`) {
		t.Fatalf("expected scheme attr in log, got: %s", out)
	}
}

func TestLogSchemeDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-scheme")
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"scheme"`) {
		t.Fatalf("scheme should not be logged by default, got: %s", buf.String())
	}
}

// --- LogResponseHeaders ---

func TestLogResponseHeaders(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:             log,
		LogResponseHeaders: []string{"X-Request-Id", "X-Trace-Id"},
		SensitiveHeaders:   []string{},
	})
	handler := func(c *celeris.Context) error {
		c.SetHeader("X-Request-Id", "req-abc")
		c.SetHeader("X-Trace-Id", "trace-123")
		// Use NoContent to avoid Blob overwriting respHeaders.
		return c.NoContent(204)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/resp-hdrs")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"resp_header.x-request-id":"req-abc"`) {
		t.Fatalf("expected resp_header.x-request-id in log, got: %s", out)
	}
	if !strings.Contains(out, `"resp_header.x-trace-id":"trace-123"`) {
		t.Fatalf("expected resp_header.x-trace-id in log, got: %s", out)
	}
}

func TestLogResponseHeadersPartialMatch(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:             log,
		LogResponseHeaders: []string{"X-Request-Id"},
		SensitiveHeaders:   []string{},
	})
	handler := func(c *celeris.Context) error {
		c.SetHeader("X-Request-Id", "req-abc")
		c.SetHeader("X-Other", "not-wanted")
		return c.NoContent(204)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/resp-partial")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"resp_header.x-request-id":"req-abc"`) {
		t.Fatalf("expected resp_header.x-request-id in log, got: %s", out)
	}
	if strings.Contains(out, `"resp_header.x-other"`) {
		t.Fatalf("x-other should not be logged when not in LogResponseHeaders, got: %s", out)
	}
}

func TestLogResponseHeadersEmpty(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:             log,
		LogResponseHeaders: []string{"X-Missing"},
		SensitiveHeaders:   []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.NoContent(204)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/resp-empty")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"resp_header.`) {
		t.Fatalf("no resp_header should appear when header absent, got: %s", out)
	}
}

func TestLogResponseHeadersDisabledByDefault(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{Output: log, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		c.SetHeader("Content-Type", "text/plain")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-resp-hdrs")
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"resp_header.`) {
		t.Fatalf("response headers should not be logged by default, got: %s", buf.String())
	}
}

// --- SensitiveFormFields ---

func TestSensitiveFormFields(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:              log,
		LogFormValues:       true,
		SensitiveFormFields: []string{"password", "secret"},
		SensitiveHeaders:    []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-redact",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("user=alice&password=hunter2&secret=s3cret")),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"form"`) {
		t.Fatalf("expected form attr in log, got: %s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("password value should be redacted, got: %s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Fatalf("secret value should be redacted, got: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("non-sensitive field 'user' should still be present, got: %s", out)
	}
	// url.Values.Encode() URL-encodes brackets: [REDACTED] → %5BREDACTED%5D
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected REDACTED placeholder in form values, got: %s", out)
	}
}

func TestSensitiveFormFieldsCaseInsensitive(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:              log,
		LogFormValues:       true,
		SensitiveFormFields: []string{"PASSWORD"},
		SensitiveHeaders:    []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-case",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("password=secret123")),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, "secret123") {
		t.Fatalf("password should be redacted case-insensitively, got: %s", out)
	}
	// url.Values.Encode() URL-encodes brackets: [REDACTED] → %5BREDACTED%5D
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected REDACTED in form output, got: %s", out)
	}
}

func TestSensitiveFormFieldsNilNoRedaction(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		LogFormValues:    true,
		SensitiveHeaders: []string{},
		// SensitiveFormFields is nil → no redaction.
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-no-redact",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("password=visible")),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "visible") {
		t.Fatalf("password value should be visible when SensitiveFormFields is nil, got: %s", out)
	}
}

func TestSensitiveFormFieldsEmptySliceNoRedaction(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:              log,
		LogFormValues:       true,
		SensitiveFormFields: []string{},
		SensitiveHeaders:    []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-empty-sensitive",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("password=visible")),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "visible") {
		t.Fatalf("password value should be visible when SensitiveFormFields is empty, got: %s", out)
	}
}

func TestSensitiveFormFieldsOnlyWhenLogFormValuesTrue(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:              log,
		LogFormValues:       false,
		SensitiveFormFields: []string{"password"},
		SensitiveHeaders:    []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-off-sensitive",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("password=secret")),
	)
	testutil.AssertNoError(t, err)
	if strings.Contains(buf.String(), `"form"`) {
		t.Fatalf("form should not be logged when LogFormValues is false, got: %s", buf.String())
	}
}

// --- DisableColors ---

func TestDisableColors(t *testing.T) {
	var buf bytes.Buffer
	fh := NewFastHandler(&buf, &FastHandlerOptions{Color: true})
	log := slog.New(fh)
	mw := New(Config{Output: log, DisableColors: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-color")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, "\033[") {
		t.Fatalf("expected no ANSI escape codes with DisableColors, got: %s", out)
	}
}

func TestDisableColorsFalsePreservesColor(t *testing.T) {
	var buf bytes.Buffer
	fh := NewFastHandler(&buf, &FastHandlerOptions{Color: true})
	log := slog.New(fh)
	mw := New(Config{Output: log, DisableColors: false, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/with-color")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "\033[") {
		t.Fatalf("expected ANSI escape codes when DisableColors is false, got: %s", out)
	}
}

func TestDisableColorsGroupHandler(t *testing.T) {
	var buf bytes.Buffer
	fh := NewFastHandler(&buf, &FastHandlerOptions{Color: true})
	gh := fh.WithGroup("grp")
	log := slog.New(gh)
	mw := New(Config{Output: log, DisableColors: true, SensitiveHeaders: []string{}})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/no-color-grp")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, "\033[") {
		t.Fatalf("expected no ANSI codes with DisableColors on groupHandler, got: %s", out)
	}
}

// --- CLFConfig ---

func TestCLFConfig(t *testing.T) {
	cfg := CLFConfig()
	if !cfg.LogHost {
		t.Fatal("CLFConfig should enable LogHost")
	}
	if !cfg.LogUserAgent {
		t.Fatal("CLFConfig should enable LogUserAgent")
	}
	if !cfg.LogReferer {
		t.Fatal("CLFConfig should enable LogReferer")
	}
	if cfg.Fields == nil {
		t.Fatal("CLFConfig should set Fields callback")
	}
}

func TestCLFConfigFieldsOutput(t *testing.T) {
	cfg := CLFConfig()
	// Override output to capture.
	var buf bytes.Buffer
	cfg.Output = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.SensitiveHeaders = []string{}
	mw := New(cfg)
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/clf",
		celeristest.WithHeader("user-agent", "TestBot/1.0"),
		celeristest.WithHeader("referer", "http://ref.example.com"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"clf"`) {
		t.Fatalf("expected clf field in log, got: %s", out)
	}
	if !strings.Contains(out, "GET") {
		t.Fatalf("CLF line should contain method, got: %s", out)
	}
}

// --- JSONConfig ---

func TestJSONConfig(t *testing.T) {
	cfg := JSONConfig()
	if cfg.Output == nil {
		t.Fatal("JSONConfig should set Output")
	}
	if !cfg.LogHost {
		t.Fatal("JSONConfig should enable LogHost")
	}
	if !cfg.LogUserAgent {
		t.Fatal("JSONConfig should enable LogUserAgent")
	}
	if !cfg.LogReferer {
		t.Fatal("JSONConfig should enable LogReferer")
	}
	if !cfg.LogRoute {
		t.Fatal("JSONConfig should enable LogRoute")
	}
	if !cfg.LogQueryParams {
		t.Fatal("JSONConfig should enable LogQueryParams")
	}
}

func TestJSONConfigUsable(t *testing.T) {
	cfg := JSONConfig()
	// Override output to capture.
	var buf bytes.Buffer
	cfg.Output = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.SensitiveHeaders = []string{}
	mw := New(cfg)
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/json-config")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"method":"GET"`) {
		t.Fatalf("expected JSON output with method, got: %s", out)
	}
}

// --- Fuzz parseCookieNames ---

func FuzzParseCookieNames(f *testing.F) {
	f.Add("session=abc; theme=dark")
	f.Add("a=1; b=2;")
	f.Add("bare")
	f.Add("")
	f.Add("  ; ; ;  ")
	f.Add("=noname; good=val")
	f.Add(strings.Repeat("k=v; ", 1000))
	f.Fuzz(func(t *testing.T, raw string) {
		result := parseCookieNames(raw)
		// Must not panic (implicit).
		// Invariant: result never contains '=' (names are before '=').
		if strings.Contains(result, "=") {
			t.Errorf("parseCookieNames(%q) contains '=': %s", raw, result)
		}
		// Invariant: empty input → empty result.
		trimmed := strings.TrimFunc(raw, func(r rune) bool {
			return r == ' ' || r == ';'
		})
		if trimmed == "" && result != "" {
			t.Errorf("parseCookieNames(%q) should be empty for blank input, got: %s", raw, result)
		}
	})
}

// --- validate() panic ---

func TestValidatePanicNegativeMaxCapture(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected validate() to panic on negative MaxCaptureBytes")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T: %v", r, r)
		}
		if !strings.Contains(msg, "MaxCaptureBytes") {
			t.Fatalf("panic message should mention MaxCaptureBytes, got: %s", msg)
		}
	}()
	cfg := Config{
		CaptureRequestBody: true,
		MaxCaptureBytes:    -1,
	}
	cfg.validate()
}

func TestValidateNoPanicWhenCaptureDisabled(t *testing.T) {
	// Negative MaxCaptureBytes should NOT panic when capture is disabled.
	cfg := Config{
		CaptureRequestBody:  false,
		CaptureResponseBody: false,
		MaxCaptureBytes:     -1,
	}
	cfg.validate() // must not panic
}

// --- DefaultSensitiveFormFields ---

func TestDefaultSensitiveFormFields(t *testing.T) {
	got := DefaultSensitiveFormFields()
	if len(got) == 0 {
		t.Fatal("DefaultSensitiveFormFields() should not be empty")
	}
	expected := map[string]bool{
		"password":      true,
		"passwd":        true,
		"secret":        true,
		"token":         true,
		"access_token":  true,
		"refresh_token": true,
		"credit_card":   true,
		"card_number":   true,
		"cvv":           true,
		"ssn":           true,
	}
	for _, f := range got {
		if !expected[f] {
			t.Fatalf("unexpected field in DefaultSensitiveFormFields(): %s", f)
		}
	}
	if len(got) != len(expected) {
		t.Fatalf("DefaultSensitiveFormFields() has %d entries, want %d", len(got), len(expected))
	}
	// Mutation safety.
	got[0] = "MUTATED"
	got2 := DefaultSensitiveFormFields()
	for _, f := range got2 {
		if f == "MUTATED" {
			t.Fatal("DefaultSensitiveFormFields() returned a shared slice")
		}
	}
}

func TestDefaultSensitiveFormFieldsRedaction(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:              log,
		LogFormValues:       true,
		SensitiveFormFields: DefaultSensitiveFormFields(),
		SensitiveHeaders:    []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "POST", "/form-defaults",
		celeristest.WithHeader("content-type", "application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte("username=alice&password=hunter2&ssn=123-45-6789")),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, "alice") {
		t.Fatalf("non-sensitive field should be visible, got: %s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("password should be redacted with DefaultSensitiveFormFields, got: %s", out)
	}
	if strings.Contains(out, "123-45-6789") {
		t.Fatalf("ssn should be redacted with DefaultSensitiveFormFields, got: %s", out)
	}
}

// --- LogContextKeys ---

func TestLogContextKeys(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		LogContextKeys:   []string{"tenant_id", "user_id"},
		SensitiveHeaders: []string{},
	})
	handler := func(c *celeris.Context) error {
		c.Set("tenant_id", "acme")
		c.Set("user_id", "u-42")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/ctx-keys")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"ctx.tenant_id":"acme"`) {
		t.Fatalf("expected ctx.tenant_id in log, got: %s", out)
	}
	if !strings.Contains(out, `"ctx.user_id":"u-42"`) {
		t.Fatalf("expected ctx.user_id in log, got: %s", out)
	}
}

func TestLogContextKeysNonString(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		LogContextKeys:   []string{"count"},
		SensitiveHeaders: []string{},
	})
	handler := func(c *celeris.Context) error {
		c.Set("count", 42)
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/ctx-int")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"ctx.count":"42"`) {
		t.Fatalf("expected ctx.count with fmt.Sprint value, got: %s", out)
	}
}

func TestLogContextKeysMissing(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		LogContextKeys:   []string{"missing_key"},
		SensitiveHeaders: []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/ctx-missing")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"ctx.missing_key"`) {
		t.Fatalf("missing key should be omitted, got: %s", out)
	}
}

func TestLogContextKeysEmpty(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		SensitiveHeaders: []string{},
	})
	handler := func(c *celeris.Context) error {
		c.Set("something", "val")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/ctx-empty")
	testutil.AssertNoError(t, err)
	out := buf.String()
	if strings.Contains(out, `"ctx.`) {
		t.Fatalf("no ctx. attrs expected when LogContextKeys is nil, got: %s", out)
	}
}

// --- Data race test ---

func TestLogger_DataRace(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := New(Config{
		Output:         log,
		LogHost:        true,
		LogUserAgent:   true,
		LogReferer:     true,
		LogScheme:      true,
		LogRoute:       true,
		LogPID:         true,
		LogQueryParams: true,
		LogBytesIn:     true,
		LogContextKeys: []string{"rid"},
	})
	handler := func(c *celeris.Context) error {
		c.Set("rid", "r-123")
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				ctx, _ := celeristest.NewContext("GET", "/race",
					celeristest.WithHeader("user-agent", "RaceTest/1.0"),
					celeristest.WithHeader("referer", "https://race.example.com"),
				)
				_ = chain[0](ctx)
				celeristest.ReleaseContext(ctx)
			}
		}()
	}
	wg.Wait()
}

// discardWriter is an io.Writer that discards all data (like io.Discard but
// usable as a concrete type for the race test).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// --- v1.2.4 celeristest option tests ---

func TestLogIncludesFullPath(t *testing.T) {
	log, buf := newTestLogger()
	mw := New(Config{
		Output:           log,
		LogRoute:         true,
		SensitiveHeaders: []string{},
	})
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/items/99",
		celeristest.WithFullPath("/items/:id"),
	)
	testutil.AssertNoError(t, err)
	out := buf.String()
	if !strings.Contains(out, `"route":"/items/:id"`) {
		t.Fatalf("expected route=/items/:id in log output, got: %s", out)
	}
}
