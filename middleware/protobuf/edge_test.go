package protobuf

import (
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/goceleris/celeris/celeristest"
)

// Test edge cases in content-type detection
func TestContentTypeEdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		ct    string
		valid bool
	}{
		// Whitespace variations
		{"leading space", " application/x-protobuf", true},
		{"trailing space", "application/x-protobuf ", true},
		{"both spaces", " application/x-protobuf ", true},
		{"internal space", "application / x-protobuf", false}, // Should fail

		// Case variations with parameters
		{"mixed case with param", "APPLICATION/X-PROTOBUF; CHARSET=UTF-8", true},

		// Multiple semicolons
		{"double semicolon", "application/x-protobuf;; charset=utf-8", true},

		// Empty after semicolon
		{"empty after semi", "application/x-protobuf;", true},

		// Whitespace around semicolon
		{"space before semi", "application/x-protobuf ; charset=utf-8", true},
		{"space after semi", "application/x-protobuf; charset=utf-8", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isProtoBufContentType(tt.ct)
			if got != tt.valid {
				t.Errorf("isProtoBufContentType(%q) = %v, want %v", tt.ct, got, tt.valid)
			}
		})
	}
}

// Test Respond with edge cases
func TestRespondEdgeCases(t *testing.T) {
	// Test with no Accept header - should use first offer
	msg := wrapperspb.String("test")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test")
	// No Accept header set
	if err := Respond(ctx, 200, msg, nil); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// Should default to protobuf (first offer)
	if rec.Header("content-type") != ContentType {
		t.Errorf("expected protobuf without Accept header, got %q", rec.Header("content-type"))
	}
}

// TestRespondQZeroExclusion table-tests Accept-header q=0 exclusion of
// each protobuf content-type. RFC 7231 §5.3.1: q=0 means "not
// acceptable" and the server must NOT pick that media type.
func TestRespondQZeroExclusion(t *testing.T) {
	msg := wrapperspb.String("hello")
	cases := []struct {
		name   string
		accept string
		wantCT string
	}{
		{
			name:   "exclude application/x-protobuf forces alt",
			accept: "application/x-protobuf;q=0, application/protobuf, application/json",
			wantCT: ContentTypeAlt,
		},
		{
			name:   "exclude both protobuf variants forces JSON",
			accept: "application/x-protobuf;q=0, application/protobuf;q=0, application/json",
			wantCT: "application/json",
		},
		{
			name:   "wildcard q=0 + explicit json forces JSON",
			accept: "*/*;q=0, application/json",
			wantCT: "application/json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rec := celeristest.NewContextT(t, "GET", "/api",
				celeristest.WithHeader("accept", tc.accept))
			if err := Respond(ctx, 200, msg, map[string]string{"v": "hello"}); err != nil {
				t.Fatalf("Respond: %v", err)
			}
			got := rec.Header("content-type")
			if got != tc.wantCT {
				t.Errorf("Accept=%q → Content-Type=%q, want %q", tc.accept, got, tc.wantCT)
			}
		})
	}
}
