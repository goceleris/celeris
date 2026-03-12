package celeris

import (
	"fmt"
	"strings"
	"testing"
)

func TestHTTPErrorType(t *testing.T) {
	he := NewHTTPError(404, "not found")
	if he.Code != 404 {
		t.Fatalf("expected 404, got %d", he.Code)
	}
	if he.Message != "not found" {
		t.Fatalf("expected 'not found', got %s", he.Message)
	}
	if he.Error() != "code=404, message=not found" {
		t.Fatalf("unexpected Error(): %s", he.Error())
	}

	// With wrapped error.
	inner := fmt.Errorf("db error")
	he.Err = inner
	if he.Unwrap() != inner {
		t.Fatal("expected Unwrap to return inner error")
	}
}

func TestHTTPErrorWithError(t *testing.T) {
	inner := fmt.Errorf("database connection failed")
	he := NewHTTPError(500, "internal error").WithError(inner)

	if he.Code != 500 {
		t.Fatalf("expected 500, got %d", he.Code)
	}
	if he.Message != "internal error" {
		t.Fatalf("expected 'internal error', got %s", he.Message)
	}
	if he.Err != inner {
		t.Fatal("expected wrapped error")
	}
	if he.Unwrap() != inner {
		t.Fatal("Unwrap should return inner error")
	}

	// Error() should include the wrapped error.
	errStr := he.Error()
	if !strings.Contains(errStr, "database connection failed") {
		t.Fatalf("expected error string to contain inner error, got %s", errStr)
	}
	if !strings.Contains(errStr, "code=500") {
		t.Fatalf("expected error string to contain code, got %s", errStr)
	}
}

func TestAbortWithStatusReturnsError(t *testing.T) {
	s, rw := newTestStream("GET", "/abort-status")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.AbortWithStatus(403)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if rw.status != 403 {
		t.Fatalf("expected 403, got %d", rw.status)
	}
	if !c.IsAborted() {
		t.Fatal("expected aborted")
	}
}

func TestNextErrorPropagation(t *testing.T) {
	s, _ := newTestStream("GET", "/err")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	handlerErr := fmt.Errorf("handler failed")
	c.handlers = []HandlerFunc{
		func(_ *Context) error {
			return handlerErr
		},
	}

	err := c.Next()
	if err != handlerErr {
		t.Fatalf("expected handlerErr, got %v", err)
	}
}

func TestNextErrorShortCircuit(t *testing.T) {
	s, _ := newTestStream("GET", "/short")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	secondCalled := false
	c.handlers = []HandlerFunc{
		func(_ *Context) error {
			return fmt.Errorf("stop")
		},
		func(_ *Context) error {
			secondCalled = true
			return nil
		},
	}

	err := c.Next()
	if err == nil {
		t.Fatal("expected error")
	}
	if secondCalled {
		t.Fatal("second handler should not have been called after error")
	}
}

func TestMiddlewareErrorSwallow(t *testing.T) {
	s, _ := newTestStream("GET", "/swallow")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.handlers = []HandlerFunc{
		func(c *Context) error {
			err := c.Next()
			if err != nil {
				// Middleware swallows the error.
				return nil
			}
			return nil
		},
		func(_ *Context) error {
			return fmt.Errorf("downstream error")
		},
	}

	err := c.Next()
	if err != nil {
		t.Fatalf("expected nil after swallow, got %v", err)
	}
}
