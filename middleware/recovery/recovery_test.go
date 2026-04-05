package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// errBrokenPipe implements error and wraps syscall.EPIPE for testing.
type errBrokenPipe struct{}

func (e *errBrokenPipe) Error() string { return "broken pipe" }
func (e *errBrokenPipe) Is(target error) bool {
	return target == syscall.EPIPE
}

// errConnAborted implements error and wraps syscall.ECONNABORTED for testing.
type errConnAborted struct{}

func (e *errConnAborted) Error() string { return "connection aborted" }
func (e *errConnAborted) Is(target error) bool {
	return target == syscall.ECONNABORTED
}

func TestRecoveryPanicTypes(t *testing.T) {
	tests := []struct {
		name     string
		panicVal any
		wantCode int
		wantBody string
	}{
		{"string panic", "test panic", 500, "Internal Server Error"},
		{"error panic", errors.New("error panic"), 500, "Internal Server Error"},
		{"int panic", 42, 500, "Internal Server Error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New()
			handler := func(_ *celeris.Context) error { panic(tt.panicVal) }
			chain := []celeris.HandlerFunc{mw, handler}
			rec, err := testutil.RunChain(t, chain, "GET", "/panic")
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, tt.wantCode)
			testutil.AssertBodyContains(t, rec, tt.wantBody)
		})
	}
}

func TestRecoveryNoPanic(t *testing.T) {
	mw := New()
	handler := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/ok")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestRecoverySkip(t *testing.T) {
	mw := New(Config{
		Skip: func(_ *celeris.Context) bool { return true },
	})
	handler := func(_ *celeris.Context) error {
		panic("should not be caught")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic to propagate when skip=true")
		}
	}()
	_, _ = testutil.RunChain(t, chain, "GET", "/skip")
}

func TestRecoveryCustomHandler(t *testing.T) {
	mw := New(Config{
		ErrorHandler: func(c *celeris.Context, err any) error {
			return c.String(503, "custom: %v", err)
		},
	})
	handler := func(_ *celeris.Context) error {
		panic("boom")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/custom")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)
	testutil.AssertBodyContains(t, rec, "custom: boom")
}

func TestRecoveryLogsStack(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Logger: log, StackSize: 4096})
	handler := func(_ *celeris.Context) error {
		panic("stack test")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/stack")
	if !strings.Contains(buf.String(), "stack test") {
		t.Fatalf("expected panic value in log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "goroutine") {
		t.Fatalf("expected stack trace in log: %s", buf.String())
	}
}

func TestRecoveryNoStack(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Logger: log, DisableLogStack: true})
	handler := func(_ *celeris.Context) error {
		panic("no stack")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/nostack")
	if strings.Contains(buf.String(), "goroutine") {
		t.Fatalf("expected no stack trace: %s", buf.String())
	}
}

func TestRecoveryPassesError(t *testing.T) {
	mw := New()
	handler := func(_ *celeris.Context) error {
		return celeris.NewHTTPError(400, "bad request")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/error")
	testutil.AssertHTTPError(t, err, 400)
}

func TestErrAbortHandlerRepanics(t *testing.T) {
	mw := New(Config{DisableLogStack: true})
	handler := func(_ *celeris.Context) error {
		panic(http.ErrAbortHandler)
	}
	chain := []celeris.HandlerFunc{mw, handler}
	defer func() {
		r := recover()
		if r != http.ErrAbortHandler {
			t.Fatalf("expected http.ErrAbortHandler re-panic, got %v", r)
		}
	}()
	_, _ = testutil.RunChain(t, chain, "GET", "/abort")
	t.Fatal("expected panic, not normal return")
}

func TestRecoveryLogsMethodAndPath(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		stackSize int
		panicVal  string
		wantStack bool
	}{
		{"with stack", "POST", "/test-path", 4096, "method path test", true},
		{"no stack", "DELETE", "/delete-path", 0, "no stack method path", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			log := slog.New(slog.NewJSONHandler(buf, nil))
			mw := New(Config{Logger: log, StackSize: tt.stackSize})
			handler := func(_ *celeris.Context) error { panic(tt.panicVal) }
			chain := []celeris.HandlerFunc{mw, handler}
			_, _ = testutil.RunChain(t, chain, tt.method, tt.path)
			output := buf.String()
			if !strings.Contains(output, fmt.Sprintf(`"method":"%s"`, tt.method)) {
				t.Fatalf("expected method in log: %s", output)
			}
			if !strings.Contains(output, fmt.Sprintf(`"path":"%s"`, tt.path)) {
				t.Fatalf("expected path in log: %s", output)
			}
			if tt.wantStack && !strings.Contains(output, "goroutine") {
				t.Fatalf("expected stack trace in log: %s", output)
			}
			if !tt.wantStack && strings.Contains(output, "goroutine") {
				t.Fatalf("expected no stack trace with StackSize=0, got: %s", output)
			}
		})
	}
}

func TestStackAllCapturesAllGoroutines(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Logger: log, StackSize: 65536, StackAll: true})

	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(ready)
		<-done
	}()
	<-ready

	handler := func(_ *celeris.Context) error {
		panic("stack all test")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/stackall")
	close(done)

	output := buf.String()
	count := strings.Count(output, "goroutine")
	if count < 2 {
		t.Fatalf("expected multiple goroutine stacks with StackAll=true, found %d occurrences in: %s", count, output)
	}
}

func TestNestedRecoveryPanic(t *testing.T) {
	mw := New(Config{
		ErrorHandler: func(_ *celeris.Context, _ any) error {
			panic("error handler panic")
		},
	})
	handler := func(_ *celeris.Context) error {
		panic("original panic")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/nested")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 500)
	body := rec.BodyString()
	if !strings.Contains(body, "Internal Server Error") {
		t.Fatalf("expected default JSON error body, got: %s", body)
	}
}

func TestBrokenPipeHandlerReceivesError(t *testing.T) {
	var receivedErr any
	mw := New(Config{
		DisableLogStack: true,
		BrokenPipeHandler: func(c *celeris.Context, err any) error {
			receivedErr = err
			return c.NoContent(499)
		},
	})
	handler := func(_ *celeris.Context) error {
		panic(&net.OpError{Op: "write", Err: &errBrokenPipe{}})
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/broken")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 499)
	if receivedErr == nil {
		t.Fatal("expected BrokenPipeHandler to receive the error")
	}
	opErr, ok := receivedErr.(*net.OpError)
	if !ok {
		t.Fatalf("expected *net.OpError, got %T", receivedErr)
	}
	if opErr.Op != "write" {
		t.Fatalf("expected Op=write, got %q", opErr.Op)
	}
	if !errors.Is(opErr.Err, syscall.EPIPE) {
		t.Fatalf("expected EPIPE wrapped error, got %v", opErr.Err)
	}
}

func TestBrokenPipeNilHandlerReturnsError(t *testing.T) {
	tests := []struct {
		name     string
		panicVal any
	}{
		{"EPIPE via OpError", &net.OpError{Op: "write", Err: &errBrokenPipe{}}},
		{"EPIPE direct", &errBrokenPipe{}},
		{"ECONNABORTED direct", &errConnAborted{}},
		{"ECONNABORTED via OpError", &net.OpError{Op: "write", Err: &errConnAborted{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New(Config{DisableLogStack: true})
			handler := func(_ *celeris.Context) error { panic(tt.panicVal) }
			chain := []celeris.HandlerFunc{mw, handler}
			_, err := testutil.RunChain(t, chain, "GET", "/broken")
			if err == nil {
				t.Fatal("expected error from broken pipe")
			}
			if !errors.Is(err, ErrBrokenPipe) {
				t.Fatalf("expected ErrBrokenPipe sentinel, got: %v", err)
			}
			if !strings.Contains(err.Error(), "broken pipe") {
				t.Fatalf("expected 'broken pipe' in error message, got: %v", err)
			}
		})
	}
}

func TestNegativeStackSizeSilentlyFixed(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Logger: log, StackSize: -1})
	handler := func(_ *celeris.Context) error {
		panic("negative stack")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/neg-stack")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 500)
	if !strings.Contains(buf.String(), "goroutine") {
		t.Fatalf("expected stack trace after negative StackSize was fixed, got: %s", buf.String())
	}
}

func TestNestedRecoveryLogs(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{
		Logger: log,
		ErrorHandler: func(_ *celeris.Context, _ any) error {
			panic("handler exploded")
		},
	})
	handler := func(_ *celeris.Context) error {
		panic("original")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/nested-log")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 500)
	output := buf.String()
	if !strings.Contains(output, "handler exploded") {
		t.Fatalf("expected nested panic value in log, got: %s", output)
	}
	if !strings.Contains(output, "panic in error handler") {
		t.Fatalf("expected 'panic in error handler' message, got: %s", output)
	}
}

func TestBrokenPipeLogging(t *testing.T) {
	tests := []struct {
		name       string
		disableLog bool
		wantLog    bool
	}{
		{"log enabled by default", false, true},
		{"log suppressed", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			log := slog.New(slog.NewJSONHandler(buf, nil))
			mw := New(Config{Logger: log, DisableBrokenPipeLog: tt.disableLog})
			handler := func(_ *celeris.Context) error {
				panic(&net.OpError{Op: "write", Err: &errBrokenPipe{}})
			}
			chain := []celeris.HandlerFunc{mw, handler}
			_, _ = testutil.RunChain(t, chain, "GET", "/broken")
			if tt.wantLog && !strings.Contains(buf.String(), "broken pipe") {
				t.Fatalf("expected broken pipe log, got: %s", buf.String())
			}
			if !tt.wantLog && buf.Len() > 0 {
				t.Fatalf("expected no log, got: %s", buf.String())
			}
		})
	}
}

func TestDisableBrokenPipeLogStillLogsNormalPanics(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Logger: log, DisableBrokenPipeLog: true})
	handler := func(_ *celeris.Context) error {
		panic("normal panic")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/normal-panic")
	if buf.Len() == 0 {
		t.Fatal("expected normal panics to still be logged when DisableBrokenPipeLog=true")
	}
	if !strings.Contains(buf.String(), "normal panic") {
		t.Fatalf("expected 'normal panic' in log, got: %s", buf.String())
	}
}

func TestDisableLogStackBehavior(t *testing.T) {
	tests := []struct {
		name      string
		disable   bool
		stackSize int
		wantStack bool
		wantLog   bool
	}{
		{"zero-value logs stack", false, 4096, true, true},
		{"true suppresses all logging", true, 4096, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			log := slog.New(slog.NewJSONHandler(buf, nil))
			mw := New(Config{Logger: log, StackSize: tt.stackSize, DisableLogStack: tt.disable})
			handler := func(_ *celeris.Context) error { panic("test") }
			chain := []celeris.HandlerFunc{mw, handler}
			_, _ = testutil.RunChain(t, chain, "GET", "/test")
			hasStack := strings.Contains(buf.String(), "goroutine")
			if tt.wantStack && !hasStack {
				t.Fatalf("expected stack trace, got: %s", buf.String())
			}
			if !tt.wantLog && buf.Len() > 0 {
				t.Fatalf("expected no log, got: %s", buf.String())
			}
		})
	}
}

func TestPanicAfterResponseWritten(t *testing.T) {
	mw := New(Config{DisableLogStack: true})
	handler := func(c *celeris.Context) error {
		_ = c.String(200, "ok")
		panic("late panic")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/written-then-panic")
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
	testutil.AssertError(t, err)
	if !errors.Is(err, ErrPanicResponseCommitted) {
		t.Fatalf("expected ErrPanicResponseCommitted in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "response committed") {
		t.Fatalf("expected 'response committed' in error, got: %v", err)
	}
}

func TestPanicAfterContextCancelled(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	mw := New(Config{Logger: log})
	handler := func(c *celeris.Context) error {
		ctx, cancel := context.WithCancel(c.Context())
		cancel()
		c.SetContext(ctx)
		panic("cancelled panic")
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/cancelled-panic")
	testutil.AssertError(t, err)
	if !errors.Is(err, ErrPanicContextCancelled) {
		t.Fatalf("expected ErrPanicContextCancelled in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "recovery: panic (context cancelled)") {
		t.Fatalf("expected 'recovery: panic (context cancelled)' in error, got: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "panic after context cancelled") {
		t.Fatalf("expected 'panic after context cancelled' in log, got: %s", output)
	}
}

func TestLogLevelInfoNotOverridden(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := New(Config{Logger: log, LogLevel: slog.LevelInfo})
	handler := func(_ *celeris.Context) error { panic("info level test") }
	chain := []celeris.HandlerFunc{mw, handler}
	_, _ = testutil.RunChain(t, chain, "GET", "/info-level")
	output := buf.String()
	if !strings.Contains(output, `"level":"INFO"`) {
		t.Fatalf("expected INFO level in log (LevelInfo should not be overridden to ERROR), got: %s", output)
	}
	if strings.Contains(output, `"level":"ERROR"`) {
		t.Fatalf("LogLevel=slog.LevelInfo was overridden to ERROR: %s", output)
	}
}

func TestLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		level     slog.Level
		wantLevel string
	}{
		{"custom WARN", slog.LevelWarn, `"level":"WARN"`},
		{"default ERROR", slog.LevelError, `"level":"ERROR"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			mw := New(Config{Logger: log, StackSize: 4096, LogLevel: tt.level})
			handler := func(_ *celeris.Context) error { panic("level test") }
			chain := []celeris.HandlerFunc{mw, handler}
			_, _ = testutil.RunChain(t, chain, "GET", "/log-level")
			if !strings.Contains(buf.String(), tt.wantLevel) {
				t.Fatalf("expected %s level in log, got: %s", tt.wantLevel, buf.String())
			}
		})
	}
}

func FuzzRecoveryPanicValues(f *testing.F) {
	f.Add("string panic")
	f.Add("")
	f.Fuzz(func(t *testing.T, val string) {
		mw := New(Config{DisableLogStack: true})
		handler := func(_ *celeris.Context) error {
			panic(val)
		}
		chain := []celeris.HandlerFunc{mw, handler}
		rec, err := testutil.RunChain(t, chain, "GET", "/fuzz")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.StatusCode != 500 {
			t.Fatalf("expected 500, got %d", rec.StatusCode)
		}
	})
}
