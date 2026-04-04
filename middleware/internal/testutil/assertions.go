// Package testutil provides test helpers and assertions for middleware tests.
package testutil

import (
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// AssertStatus fails the test if the recorder's status code does not match expected.
func AssertStatus(t *testing.T, rec *celeristest.ResponseRecorder, expected int) {
	t.Helper()
	if rec.StatusCode != expected {
		t.Fatalf("status: got %d, want %d", rec.StatusCode, expected)
	}
}

// AssertHeader fails the test if the header value does not match expected.
func AssertHeader(t *testing.T, rec *celeristest.ResponseRecorder, key, expected string) {
	t.Helper()
	got := rec.Header(key)
	if got != expected {
		t.Fatalf("header %q: got %q, want %q", key, got, expected)
	}
}

// AssertHeaderContains fails the test if no header entry for key contains substr.
// When multiple entries exist for the same key (e.g., from AddHeader), all
// entries are checked and the assertion passes if any one contains substr.
func AssertHeaderContains(t *testing.T, rec *celeristest.ResponseRecorder, key, substr string) {
	t.Helper()
	for _, h := range rec.Headers {
		if h[0] == key && strings.Contains(h[1], substr) {
			return
		}
	}
	t.Fatalf("header %q: no entry contains %q (entries: %v)", key, substr, allHeaderValues(rec, key))
}

// allHeaderValues returns all values for the given header key.
func allHeaderValues(rec *celeristest.ResponseRecorder, key string) []string {
	var vals []string
	for _, h := range rec.Headers {
		if h[0] == key {
			vals = append(vals, h[1])
		}
	}
	return vals
}

// AssertNoHeader fails the test if any header entry exists for key.
func AssertNoHeader(t *testing.T, rec *celeristest.ResponseRecorder, key string) {
	t.Helper()
	for _, h := range rec.Headers {
		if h[0] == key {
			t.Fatalf("header %q: expected absent, got %q", key, h[1])
		}
	}
}

// AssertBodyContains fails the test if the body does not contain substr.
func AssertBodyContains(t *testing.T, rec *celeristest.ResponseRecorder, substr string) {
	t.Helper()
	body := rec.BodyString()
	if !strings.Contains(body, substr) {
		t.Fatalf("body does not contain %q: %q", substr, body)
	}
}

// AssertBodyEmpty fails the test if the body is not empty.
func AssertBodyEmpty(t *testing.T, rec *celeristest.ResponseRecorder) {
	t.Helper()
	if len(rec.Body) > 0 {
		t.Fatalf("body: expected empty, got %d bytes", len(rec.Body))
	}
}

// AssertNoError fails the test if err is not nil.
func AssertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// AssertError fails the test if err is nil.
func AssertError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// AssertHTTPError fails the test if err is not an *celeris.HTTPError with the expected code.
func AssertHTTPError(t *testing.T, err error, expectedCode int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected HTTPError with code %d, got nil", expectedCode)
	}
	var he *celeris.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Code != expectedCode {
		t.Fatalf("HTTPError code: got %d, want %d", he.Code, expectedCode)
	}
}
