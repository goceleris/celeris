//go:build validation

package session

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/validation"
)

// TestLateMutationBumpsValidationCounter confirms the validation build
// mirrors a dropped session cookie into validation.SessionCookieDrops so
// probatorium's validator can observe it over the snapshot socket.
func TestLateMutationBumpsValidationCounter(t *testing.T) {
	mw := New(Config{Store: NewMemoryStore()})
	before := validation.SessionCookieDrops.Load()
	h := func(c *celeris.Context) error {
		if err := c.JSON(200, "ok"); err != nil {
			return err
		}
		FromContext(c).Set("user", "late")
		return nil
	}
	ctx, _ := celeristest.NewContextT(t, "POST", "/late", celeristest.WithHandlers(mw, h))
	if err := ctx.Next(); err != nil {
		t.Fatal(err)
	}
	if after := validation.SessionCookieDrops.Load(); after != before+1 {
		t.Fatalf("SessionCookieDrops: got %d, want %d", after, before+1)
	}
}
