package postgres

// celeris#1212, the unset case: with no connect_timeout in the DSN the dial and
// the startup have a default budget (10 s); connect_timeout=0 written in the DSN
// keeps the libpq meaning, no client-side timeout. The tests lower the default
// through testHookDefaultConnectTimeout so they do not wait 10 s.

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// dsnForms returns the DSN of srv written both ways, with extra appended
// ("connect_timeout=1" or "").
func dsnForms(t *testing.T, srv *stalledPG, extra string) map[string]DSN {
	t.Helper()
	host, port, _ := net.SplitHostPort(srv.addr)
	kv := "host=" + host + " port=" + port + " user=u dbname=d sslmode=disable"
	url := "postgres://u@" + host + ":" + port + "/d?sslmode=disable"
	if extra != "" {
		kv += " " + extra
		url += "&" + extra
	}
	out := map[string]DSN{}
	for name, raw := range map[string]string{"keyvalue": kv, "url": url} {
		d, err := ParseDSN(raw)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = d
	}
	return out
}

func lowerDefaultConnectTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	testHookDefaultConnectTimeout.Store(&d)
	t.Cleanup(func() { testHookDefaultConnectTimeout.Store(nil) })
}

// No connect_timeout in the DSN, a context without a deadline, a server that
// accepts and never answers: the dial fails after the default, on every dial path
// and for both DSN forms. (Main: for ever.)
func TestStartupIsBoundedByTheDefaultConnectTimeout1212(t *testing.T) {
	const def = 300 * time.Millisecond
	lowerDefaultConnectTimeout(t, def)
	for _, m := range startupDialModes {
		for _, form := range []string{"keyvalue", "url"} {
			t.Run(m.name+"/"+form, func(t *testing.T) {
				t.Parallel()
				srv := startStalledPG(t, "silent")
				dsn := dsnForms(t, srv, "")[form]
				elapsed, err := dialWithin(t, 2*time.Second, func() (*pgConn, error) {
					return m.dial(t, context.Background(), dsn)
				})
				t.Logf("%s/%s: %v after %v", m.name, form, err, elapsed.Round(10*time.Millisecond))
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("error %v does not match context.DeadlineExceeded", err)
				}
				if !strings.Contains(err.Error(), "startup timed out") {
					t.Errorf("error %q does not say that the startup timed out", err)
				}
				if elapsed < def/2 {
					t.Errorf("returned after %v, before the default of %v", elapsed, def)
				}
			})
		}
	}
}

// The pool's acquire path, through Open with a DSN that has no connect_timeout.
func TestPoolAcquireIsBoundedByTheDefaultConnectTimeout1212(t *testing.T) {
	lowerDefaultConnectTimeout(t, 300*time.Millisecond)
	srv := startStalledPG(t, "silent")
	host, port, _ := net.SplitHostPort(srv.addr)
	p, err := Open("postgres://u@" + host + ":" + port + "/d?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	done := make(chan error, 1)
	go func() { done <- p.Ping(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Ping error %v does not match context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping is still blocked after 2s: a DSN without connect_timeout has no default bound")
	}
	if n := p.Stats().Open; n != 0 {
		t.Errorf("the pool holds %d conns after the failed dial, want 0", n)
	}
}

// connect_timeout=0 written in the DSN is the libpq "no timeout": the driver sets
// no bound, only the caller's context ends the dial (here a deadline of 900 ms,
// three times the lowered default). A connect_timeout that is set wins over the
// default, either way round.
func TestExplicitConnectTimeoutOverridesTheDefault1212(t *testing.T) {
	lowerDefaultConnectTimeout(t, 300*time.Millisecond)
	for _, tc := range []struct {
		name  string
		extra string
		// wantDriver is whether the driver's own bound ends the dial; ctxFor is
		// the caller's deadline.
		wantDriver bool
		ctxFor     time.Duration
	}{
		{"zero-is-unbounded", "connect_timeout=0", false, 900 * time.Millisecond},
		{"one-second-beats-the-default", "connect_timeout=1", true, 3 * time.Second},
	} {
		for _, m := range startupDialModes {
			for _, form := range []string{"keyvalue", "url"} {
				t.Run(tc.name+"/"+m.name+"/"+form, func(t *testing.T) {
					t.Parallel()
					srv := startStalledPG(t, "silent")
					dsn := dsnForms(t, srv, tc.extra)[form]
					ctx, cancel := context.WithTimeout(context.Background(), tc.ctxFor)
					defer cancel()
					elapsed, err := dialWithin(t, 4*time.Second, func() (*pgConn, error) {
						return m.dial(t, ctx, dsn)
					})
					t.Logf("%s: %v after %v", t.Name(), err, elapsed.Round(10*time.Millisecond))
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("error %v does not match context.DeadlineExceeded", err)
					}
					driver := strings.Contains(err.Error(), "startup timed out")
					if tc.wantDriver && (!driver || elapsed < 800*time.Millisecond || elapsed > 2*time.Second) {
						t.Errorf("returned after %v with %q: want the driver's bound of the explicit connect_timeout=1, not the default (300 ms)", elapsed, err)
					}
					if !tc.wantDriver && (driver || elapsed < 800*time.Millisecond) {
						t.Errorf("returned after %v with %q: connect_timeout=0 was bounded by the driver", elapsed, err)
					}
				})
			}
		}
	}
}
