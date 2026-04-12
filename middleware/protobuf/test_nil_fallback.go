package protobuf

import (
	"testing"
	"github.com/goceleris/celeris/celeristest"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Test Respond with nil fallback when JSON is chosen
func TestRespondNilFallback(t *testing.T) {
	msg := wrapperspb.String("test")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test",
		celeristest.WithHeader("accept", "application/json"))
	
	// Pass nil as jsonFallback - what happens?
	err := Respond(ctx, 200, msg, nil)
	
	t.Logf("Error: %v", err)
	t.Logf("Content-Type: %s", rec.Header("content-type"))
	t.Logf("Body: %s", rec.BodyString())
	
	// If JSON marshaling with nil succeeds, it produces "null"
	// which might be unexpected
}
