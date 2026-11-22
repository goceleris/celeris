package postgres

// Connection poolers (PgBouncer, pgcat, Supavisor, RDS Proxy) answer a startup
// parameter they do not track with a FATAL ErrorResponse and close the
// connection. PgBouncer 1.26.1 does it for extra_float_digits and bytea_output
// and accepts DateStyle, unless ignore_startup_parameters lists them. The
// startup pins (startupPins) must not make such a server unreachable: a pin the
// server rejects is dropped and the connection is retried, once per pin at most
// and remembered per address; startup_pins=off in the DSN sends none.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// fakePooler is a server that behaves like a pooler for the startup: it reads each
// StartupMessage, answers a FATAL 08P01 "unsupported startup parameter: <name>"
// (PgBouncer's text) when the message carries a parameter in reject, an
// authentication failure when failAuth is set, and otherwise completes the startup.
type fakePooler struct {
	addr     string
	reject   map[string]bool // lower-case parameter names the pooler does not accept
	failAuth bool

	mu       sync.Mutex
	startups []map[string]string // every StartupMessage received, in order
}

func startFakePooler(t *testing.T, failAuth bool, reject ...string) *fakePooler {
	t.Helper()
	p := &fakePooler{reject: map[string]bool{}, failAuth: failAuth}
	for _, r := range reject {
		p.reject[strings.ToLower(r)] = true
	}
	p.addr = startFakePGBench(t, p.serve)
	return p
}

func (p *fakePooler) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c, lenBuf); err != nil {
		return
	}
	body := make([]byte, int(binary.BigEndian.Uint32(lenBuf))-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	params := map[string]string{}
	fields := strings.Split(string(body[4:]), "\x00") // skip the protocol version
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			break
		}
		params[fields[i]] = fields[i+1]
	}
	p.mu.Lock()
	p.startups = append(p.startups, params)
	p.mu.Unlock()

	if p.failAuth {
		_ = writeErrorResponse(c, "FATAL", "28P01", `password authentication failed for user "u"`)
		return
	}
	// PgBouncer names the first parameter it does not accept; the order the driver
	// wrote them in is not defined, so sort for a stable test.
	for _, name := range []string{"extra_float_digits", "bytea_output", "DateStyle", "startup_pins"} {
		for k := range params {
			if strings.EqualFold(k, name) && p.reject[strings.ToLower(k)] {
				_ = writeErrorResponse(c, "FATAL", "08P01", "unsupported startup parameter: "+k)
				return
			}
		}
	}
	_ = writeAuthOK(c)
	_ = writeBackendKeyData(c, 1, 2)
	_ = writeReadyForQuery(c, 'I')
	_, _ = io.Copy(io.Discard, c)
}

func writeErrorResponse(c net.Conn, severity, code, msg string) error {
	var p []byte
	for _, f := range [][2]string{{"S", severity}, {"V", severity}, {"C", code}, {"M", msg}} {
		p = append(append(p, f[0][0]), append([]byte(f[1]), 0)...)
	}
	p = append(p, 0)
	return writeMsg(c, protocol.BackendErrorResponse, p)
}

func (p *fakePooler) all() []map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]string(nil), p.startups...)
}

func (p *fakePooler) dsn(t *testing.T, extra string) DSN {
	t.Helper()
	host, port, _ := net.SplitHostPort(p.addr)
	raw := "postgres://u@" + host + ":" + port + "/d?sslmode=disable&connect_timeout=5"
	if extra != "" {
		raw += "&" + extra
	}
	d, err := ParseDSN(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func has(m map[string]string, name string) bool {
	for k := range m {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// PgBouncer's default: extra_float_digits and bytea_output are refused, DateStyle
// is tracked. The dial must succeed with the pins the pooler takes.
func TestStartupPinsPoolerDropsRejectedPins(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			p := startFakePooler(t, false, "extra_float_digits", "bytea_output")
			c, err := m.dial(t, context.Background(), p.dsn(t, ""))
			if err != nil {
				t.Fatalf("dial through a pooler that refuses extra_float_digits and bytea_output: %v", err)
			}
			defer func() { _ = c.Close() }()
			s := p.all()
			last := s[len(s)-1]
			if has(last, "extra_float_digits") || has(last, "bytea_output") {
				t.Errorf("the accepted startup still carries a refused parameter: %v", last)
			}
			if last["DateStyle"] != "ISO" {
				t.Errorf("the accepted startup lost DateStyle=ISO, which the pooler takes: %v", last)
			}
			if len(s) > 3 {
				t.Errorf("%d startups, want at most one per pin", len(s))
			}
		})
	}
}

// The drop is remembered per address: the second dial does not knock again.
func TestStartupPinsPoolerRemembersRejection(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			p := startFakePooler(t, false, "extra_float_digits", "bytea_output")
			dsn := p.dsn(t, "")
			c1, err := m.dial(t, context.Background(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c1.Close() }()
			before := len(p.all())
			c2, err := m.dial(t, context.Background(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c2.Close() }()
			if got := len(p.all()) - before; got != 1 {
				t.Errorf("second dial made %d startups, want 1 (the refused pins are remembered)", got)
			}
		})
	}
}

// A pooler that refuses every pin: all are dropped, the dial still succeeds.
func TestStartupPinsPoolerRefusesAllPins(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			p := startFakePooler(t, false, "extra_float_digits", "bytea_output", "DateStyle")
			c, err := m.dial(t, context.Background(), p.dsn(t, ""))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			s := p.all()
			last := s[len(s)-1]
			for _, n := range []string{"extra_float_digits", "bytea_output", "DateStyle"} {
				if has(last, n) {
					t.Errorf("accepted startup carries %s: %v", n, last)
				}
			}
		})
	}
}

// startup_pins=off sends no pin, makes one startup, and is not itself sent.
func TestStartupPinsOffInDSN(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			p := startFakePooler(t, false, "extra_float_digits", "bytea_output")
			c, err := m.dial(t, context.Background(), p.dsn(t, "startup_pins=off"))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			s := p.all()
			if len(s) != 1 {
				t.Fatalf("%d startups, want 1", len(s))
			}
			for _, n := range []string{"extra_float_digits", "bytea_output", "DateStyle", "startup_pins"} {
				if has(s[0], n) {
					t.Errorf("startup_pins=off: the startup carries %s: %v", n, s[0])
				}
			}
		})
	}
}

// A parameter the application wrote in the DSN is the application's: a pooler
// that refuses it is an error, not something to drop silently.
func TestStartupPinsPoolerKeepsExplicitDSNParam(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			p := startFakePooler(t, false, "extra_float_digits")
			_, err := m.dial(t, context.Background(), p.dsn(t, "extra_float_digits=2"))
			var pgErr *PGError
			if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "extra_float_digits") {
				t.Fatalf("err = %v, want the pooler's unsupported startup parameter error", err)
			}
			if n := len(p.all()); n != 1 {
				t.Errorf("%d startups, want 1: an explicit DSN parameter is not retried away", n)
			}
		})
	}
}

// Any other startup error is returned as it is, with no retry.
func TestStartupPinsOtherErrorsNotRetried(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			p := startFakePooler(t, true)
			_, err := m.dial(t, context.Background(), p.dsn(t, ""))
			var pgErr *PGError
			if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
				t.Fatalf("err = %v, want SQLSTATE 28P01", err)
			}
			if n := len(p.all()); n != 1 {
				t.Errorf("%d startups, want 1", n)
			}
		})
	}
}
