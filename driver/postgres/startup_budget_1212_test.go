package postgres

// celeris#1212: the startup has what the dial left of ConnectTimeout.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// ConnectTimeout bounds the dial and the startup together: the startup has what
// the dial left. A conn whose budget (counted from the start of its dial) has 150 ms
// left ends its startup then, although ConnectTimeout is 10 s. (The time a dial to
// loopback takes is too short to measure, so the conn is built with a budget that
// the dial has already used.)
func TestStartupHasWhatTheDialLeftOfConnectTimeout1212(t *testing.T) {
	srv := startStalledPG(t, "silent")
	dsn := srv.dsn(10 * time.Second)
	raw, err := net.Dial("tcp", srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	tcp := raw.(*net.TCPConn)
	c := &pgConn{
		useDirect:    true,
		tcp:          tcp,
		dsn:          dsn,
		opts:         dsn.Options,
		reader:       protocol.NewReader(),
		writer:       protocol.NewWriter(),
		bridge:       async.NewBridge(),
		pending:      make([]*pgRequest, 0, 2),
		serverParams: map[string]string{},
		// The dial started 9.85 s ago.
		connectDeadline: connectDeadline(time.Now().Add(-9850*time.Millisecond), dsn.Options),
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.doStartup(context.Background()) }()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		t.Logf("startup ended after %v: %v", elapsed.Round(10*time.Millisecond), err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not match context.DeadlineExceeded", err)
		}
		if elapsed > time.Second {
			t.Errorf("the startup went on for %v, the dial's budget had 150 ms left", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the startup is still blocked after 2s: it did not end at the budget the dial left")
	}
	if got := connectDeadline(time.Now(), Options{}); !got.IsZero() {
		t.Errorf("connectDeadline with no ConnectTimeout = %v, want the zero time", got)
	}
}
