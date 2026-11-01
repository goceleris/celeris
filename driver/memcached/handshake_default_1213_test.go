package memcached

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A client whose DialTimeout is unset gets the default (5 s) as the handshake
// bound. The test lowers the default through the test hook, so it does not wait
// 5 s, and checks that the bound is the default's, not none.
func TestBinaryHandshakeIsBoundedByTheDefaultDialTimeout1213(t *testing.T) {
	addr, _ := startSilentServer(t)
	d := 300 * time.Millisecond
	testHookDefaultHandshakeTimeout.Store(&d)
	t.Cleanup(func() { testHookDefaultHandshakeTimeout.Store(nil) })

	c, err := NewClient(addr, WithProtocol(ProtocolBinary)) // no WithDialTimeout
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	start := time.Now()
	err = within(t, 2*time.Second, "Client.Version with the default DialTimeout", func(ctx context.Context) error {
		_, err := c.Version(ctx)
		return err
	})
	wantTimeout(t, err)
	if !strings.Contains(err.Error(), "after "+d.String()) {
		t.Fatalf("err = %v, want the bound to be the default (%v)", err, d)
	}
	if el := time.Since(start); el < d {
		t.Fatalf("returned after %v, before the %v bound", el, d)
	}
}
