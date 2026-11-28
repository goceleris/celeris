package postgres

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// The release path of a connection that is idle: the transaction-status check
// that a released connection goes through (#1382) is one atomic load and two
// comparisons, so these stay flat against the code without it.

func benchFakeIdle(b *testing.B) string {
	return startFakePGBench(b, func(c net.Conn) {
		fakePGTrustStartupB(b, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ == protocol.MsgQuery {
					_ = writeCommandComplete(c, "OK")
					_ = writeReadyForQuery(c, 'I')
				}
			}
		})
	})
}

func benchPool(b *testing.B) *Pool {
	host, port, _ := net.SplitHostPort(benchFakeIdle(b))
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(1))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = p.Close() })
	return p
}

// BenchmarkPoolAcquireRelease is Pool.acquire plus Pool.release, no query.
func BenchmarkPoolAcquireRelease(b *testing.B) {
	p := benchPool(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := p.acquire(ctx)
		if err != nil {
			b.Fatal(err)
		}
		p.release(c)
	}
}

// BenchmarkPoolExecIdle is a statement through the fake server (the
// ReadyForQuery that carries the status goes through dispatch) and the release.
func BenchmarkPoolExecIdle(b *testing.B) {
	p := benchPool(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.ExecContext(ctx, "SELECT 1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResetSessionIsValidIdle is what database/sql runs when a clean
// connection goes back and is taken again.
func BenchmarkResetSessionIsValidIdle(b *testing.B) {
	p := benchPool(b)
	ctx := context.Background()
	c, err := p.acquire(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if err := c.Ping(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.ResetSession(ctx); err != nil || !c.IsValid() {
			b.Fatal(err)
		}
	}
}
