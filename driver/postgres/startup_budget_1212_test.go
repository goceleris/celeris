package postgres

// celeris#1212: the startup has what the dial left of ConnectTimeout.

import (
	"context"
	"errors"
	"net"
	"strings"
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
	if got := connectDeadline(time.Now(), Options{ConnectTimeout: -1}); !got.IsZero() {
		t.Errorf("connectDeadline with no timeout = %v, want the zero time", got)
	}
}

// The budget counts from the START of the dial: a connect that takes 400 ms of a
// 500 ms ConnectTimeout leaves the startup 100 ms, so the whole dial ends after
// about 500 ms, not 900. (A loopback connect takes no time, so the connect is
// slowed by the test hook that runs right after the clock starts. A variant that
// starts the clock after the connect fails this.) Not parallel: the hook is global.
func TestSlowConnectEatsTheConnectTimeoutBudget1212(t *testing.T) {
	const budget, slow = 500 * time.Millisecond, 400 * time.Millisecond
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			srv := startStalledPG(t, "silent")
			h := func() { time.Sleep(slow) }
			testHookAfterDialStart.Store(&h)
			t.Cleanup(func() { testHookAfterDialStart.Store(nil) })
			elapsed, err := dialWithin(t, 3*time.Second, func() (*pgConn, error) {
				return m.dial(t, context.Background(), srv.dsn(budget))
			})
			t.Logf("%s: %v after %v", m.name, err, elapsed.Round(10*time.Millisecond))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error %v does not match context.DeadlineExceeded", err)
			}
			if elapsed < budget-50*time.Millisecond {
				t.Errorf("returned after %v, before the budget of %v", elapsed, budget)
			}
			if elapsed > budget+(slow/2) {
				t.Errorf("returned after %v: the %v the connect took did not count against the budget of %v", elapsed, slow, budget)
			}
		})
	}
}

// connect_timeout is whole seconds >= 0: zero or absent is "no timeout" (libpq);
// a negative value would make every dial fail at once, so it is refused.
func TestParseDSNRejectsANegativeConnectTimeout1212(t *testing.T) {
	for _, dsn := range []string{
		"host=h user=u connect_timeout=-1",
		"postgres://u@h/d?connect_timeout=-5",
	} {
		_, err := ParseDSN(dsn)
		if err == nil || !strings.Contains(err.Error(), "connect_timeout") {
			t.Errorf("ParseDSN(%q) error = %v, want a connect_timeout error", dsn, err)
		}
	}
	for dsn, want := range map[string]time.Duration{
		"host=h user=u":                      0,
		"host=h user=u connect_timeout=0":    0,
		"postgres://u@h/d?connect_timeout=2": 2 * time.Second,
	} {
		d, err := ParseDSN(dsn)
		if err != nil || d.Options.ConnectTimeout != want {
			t.Errorf("ParseDSN(%q) = %v, %v; want ConnectTimeout %v", dsn, d.Options.ConnectTimeout, err, want)
		}
	}
}
