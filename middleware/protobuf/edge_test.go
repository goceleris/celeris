package protobuf

import (
	"testing"
	"github.com/goceleris/celeris/celeristest"
	"google.golang.org/protobuf/types/known/wrapperspb"
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
	Respond(ctx, 200, msg, nil)
	
	// Should default to protobuf (first offer)
	if rec.Header("content-type") != ContentType {
		t.Errorf("expected protobuf without Accept header, got %q", rec.Header("content-type"))
	}
}

