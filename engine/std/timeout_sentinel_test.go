package std

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// netHTTPEffective reproduces net/http's OWN resolution of the four server
// timeouts and reports what each field will actually enforce. A result <= 0
// means "no timeout": every consumption site in net/http guards with `d > 0`.
//
// Read out of go1.27 src/net/http/server.go, not guessed:
//
//	ReadTimeout        used verbatim, `d > 0`   (readRequest, :1039)
//	WriteTimeout       used verbatim, `d > 0`   (readRequest, :1042)
//	ReadHeaderTimeout  0 => ReadTimeout         (Server.readHeaderTimeout, :3752)
//	IdleTimeout        0 => ReadTimeout         (Server.idleTimeout, :3745)
//
// The two fallbacks are the whole point of celeris#594 on the std engine: the
// -1 sentinel normalises to celeris's internal "disabled" encoding of 0, and 0
// on ReadHeaderTimeout/IdleTimeout is not "disabled" to net/http, it is
// "whatever ReadTimeout says".
func netHTTPEffective(s *http.Server) (read, readHeader, write, idle time.Duration) {
	read, write = s.ReadTimeout, s.WriteTimeout
	readHeader = s.ReadHeaderTimeout
	if readHeader == 0 {
		readHeader = s.ReadTimeout
	}
	idle = s.IdleTimeout
	if idle == 0 {
		idle = s.ReadTimeout
	}
	return read, readHeader, write, idle
}

// TestTimeoutSentinelReachesEngine pins celeris#594 on the std engine: the
// documented -1 "no timeout" sentinel must survive the double normalisation
// (Server.doPrepare applies WithDefaults, then New applies it again) and reach
// both the engine config and the wrapped http.Server as genuinely disabled.
//
// "Disabled" is asserted twice per field, because the two are not the same
// claim:
//
//   - cfg.<field> == 0, celeris's internal encoding; and
//   - net/http enforces no timeout on that field, which for ReadHeaderTimeout
//     and IdleTimeout requires a NEGATIVE http.Server value — a 0 there is
//     read as "fall back to ReadTimeout" (see netHTTPEffective). The
//     mixed-with-* cases below are the ones that catch it: they disable the
//     header and idle timeouts while ReadTimeout stays enabled.
//
// The `normalised` column is what doPrepare hands the constructor; the raw
// column is a caller building the engine directly. Both must land on the same
// value.
func TestTimeoutSentinelReachesEngine(t *testing.T) {
	const explicit = 3 * time.Second

	type timeouts struct {
		read, readHeader, write, idle time.Duration
	}
	cases := []struct {
		name string
		in   timeouts
		// want is the celeris-internal encoding: 0 means disabled.
		want timeouts
	}{
		{
			name: "all-disabled",
			in:   timeouts{-1, -1, -1, -1},
			want: timeouts{0, 0, 0, 0},
		},
		{
			name: "unset",
			in:   timeouts{0, 0, 0, 0},
			want: timeouts{60 * time.Second, 10 * time.Second, 60 * time.Second, 600 * time.Second},
		},
		{
			name: "explicit",
			in:   timeouts{explicit, explicit, explicit, explicit},
			want: timeouts{explicit, explicit, explicit, explicit},
		},
		{
			// The std-engine gap: only the header and idle timeouts are
			// disabled, and ReadTimeout keeps its 60s default. With the
			// sentinel reaching http.Server as 0 this is not "disabled",
			// it is "60s" on both.
			name: "mixed-with-default-read",
			in:   timeouts{0, -1, 0, -1},
			want: timeouts{60 * time.Second, 0, 60 * time.Second, 0},
		},
		{
			// Same shape with an explicit short ReadTimeout: this is the
			// configuration TestReadHeaderTimeoutSentinelOnTheWire drives
			// over a real socket.
			name: "mixed-with-explicit-read",
			in:   timeouts{explicit, -1, explicit, -1},
			want: timeouts{explicit, 0, explicit, 0},
		},
	}

	for _, tc := range cases {
		for _, preNormalised := range []bool{false, true} {
			label := tc.name
			if preNormalised {
				label += "/via-doPrepare"
			} else {
				label += "/raw"
			}
			t.Run(label, func(t *testing.T) {
				cfg := resource.Config{
					Addr:              "127.0.0.1:0",
					ReadTimeout:       tc.in.read,
					ReadHeaderTimeout: tc.in.readHeader,
					WriteTimeout:      tc.in.write,
					IdleTimeout:       tc.in.idle,
				}
				if preNormalised {
					cfg = cfg.WithDefaults() // what Server.doPrepare does
				}
				e, err := New(cfg, stream.HandlerFunc(func(context.Context, *stream.Stream) error { return nil }))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				defer func() { _ = e.Shutdown(context.Background()) }()

				effRead, effReadHeader, effWrite, effIdle := netHTTPEffective(e.server)
				fields := []struct {
					name string
					cfg  time.Duration // celeris encoding: 0 == disabled
					srv  time.Duration // raw http.Server field
					eff  time.Duration // what net/http will enforce
					want time.Duration
				}{
					{"ReadTimeout", e.cfg.ReadTimeout, e.server.ReadTimeout, effRead, tc.want.read},
					{"ReadHeaderTimeout", e.cfg.ReadHeaderTimeout, e.server.ReadHeaderTimeout, effReadHeader, tc.want.readHeader},
					{"WriteTimeout", e.cfg.WriteTimeout, e.server.WriteTimeout, effWrite, tc.want.write},
					{"IdleTimeout", e.cfg.IdleTimeout, e.server.IdleTimeout, effIdle, tc.want.idle},
				}
				for _, f := range fields {
					if f.cfg != f.want {
						t.Errorf("cfg.%s = %v, want %v", f.name, f.cfg, f.want)
					}
					if f.want == 0 {
						// Disabled. net/http must enforce nothing on this
						// field, and the http.Server value must say so
						// unambiguously: 0 is "fall back to ReadTimeout"
						// for ReadHeaderTimeout and IdleTimeout.
						if f.eff > 0 {
							t.Errorf("%s disabled in config but net/http still enforces %v "+
								"(http.Server.%s=%v, ReadTimeout=%v)",
								f.name, f.eff, f.name, f.srv, e.server.ReadTimeout)
						}
						if f.srv >= 0 {
							t.Errorf("http.Server.%s = %v: a disabled timeout must reach net/http as a "+
								"negative duration, because 0 means \"fall back to ReadTimeout\" on "+
								"ReadHeaderTimeout and IdleTimeout", f.name, f.srv)
						}
						continue
					}
					// Enabled: the exact value, and no fallback in play.
					if f.srv != f.want {
						t.Errorf("http.Server.%s = %v, want %v", f.name, f.srv, f.want)
					}
					if f.eff != f.want {
						t.Errorf("net/http will enforce %v on %s, want %v", f.eff, f.name, f.want)
					}
				}
			})
		}
	}
}

// startEngineCfg boots the std engine on a fresh loopback listener with the
// caller's timeouts and returns its address plus a stop closure. It mirrors
// startEngine (shutdown_test.go) but does not impose its own timeouts, which
// is the entire subject here.
func startEngineCfg(t *testing.T, cfg resource.Config, h stream.Handler) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.Listener = ln
	cfg.Engine = engine.Std
	cfg.Protocol = engine.HTTP1
	// What Server.doPrepare does before handing the config to the engine
	// constructor, which then normalises a second time. Both passes are in
	// play in production; the sentinel has to survive both.
	cfg = cfg.WithDefaults()

	e, err := New(cfg, h)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}

	listenCtx, cancelListen := context.WithCancel(context.Background())
	listenDone := make(chan error, 1)
	go func() { listenDone <- e.Listen(listenCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for e.Addr() == nil {
		if time.Now().After(deadline) {
			cancelListen()
			t.Fatal("listener not ready within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return e.Addr().String(), func() {
		cancelListen()
		select {
		case <-listenDone:
		case <-time.After(5 * time.Second):
			t.Log("Listen did not return within 5s")
		}
	}
}

// okHandler answers every request with a 200 and a one-word body.
func okHandler() stream.Handler {
	return stream.HandlerFunc(func(_ context.Context, s *stream.Stream) error {
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}}, []byte("ok"))
	})
}

// dribbleHeaders writes a minimal HTTP/1.1 GET one byte at a time so the
// header block takes `total` to arrive — the canonical slowloris shape, and
// the only thing ReadHeaderTimeout guards. It stops early (returning the
// write error) if the server hangs up mid-dribble, which is exactly what an
// enforced ReadHeaderTimeout looks like from the client side.
func dribbleHeaders(conn net.Conn, total time.Duration) error {
	const req = "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"
	gap := total / time.Duration(len(req))
	for i := 0; i < len(req); i++ {
		if _, err := conn.Write([]byte(req[i : i+1])); err != nil {
			return err
		}
		time.Sleep(gap)
	}
	return nil
}

// TestReadHeaderTimeoutSentinelOnTheWire is the end-to-end half of
// celeris#594 on std: a slowloris client dribbling its header block must be
// killed when ReadHeaderTimeout is an explicit short value, and must NOT be
// killed when it is the documented -1 "no timeout" sentinel.
//
// The discriminator is ReadTimeout. In the disabled case it is left ENABLED
// and short, because net/http's Server.readHeaderTimeout() falls back to
// ReadTimeout whenever ReadHeaderTimeout is 0 (go1.27 server.go:3752) — so a
// sentinel that arrives as 0 silently reinstates a 300ms header timeout here.
// A GET has no body, and ReadTimeout's own whole-request deadline is only
// armed after the header block is parsed (server.go:1103), so it cannot kill
// this request on its own: the only deadline live during the dribble is
// readHeaderTimeout()'s.
//
// Conversely the enabled case disables ReadTimeout, so the kill can only be
// attributed to ReadHeaderTimeout.
//
// The two dribble lengths are chosen so the sentinel case is a negative
// control for BOTH bugs, not just one:
//
//   - 11s outlives celeris's own 10s ReadHeaderTimeout default, which a
//     non-idempotent WithDefaults reinstates over the sentinel (the original
//     celeris#594); against origin/main's resource/config.go this case is
//     killed at ~10s.
//   - ReadTimeout is a short 300ms, which is what net/http falls back to when
//     the sentinel reaches http.Server as 0 (the std-engine half); without
//     stdTimeout this case is killed at ~300ms.
//
// The positive control stays fast: 300ms of budget against a 1.5s dribble.
func TestReadHeaderTimeoutSentinelOnTheWire(t *testing.T) {
	const explicit = 300 * time.Millisecond

	cases := []struct {
		name       string
		readHeader time.Duration
		read       time.Duration
		dribble    time.Duration
		wantServed bool
	}{
		{
			// Positive control: the timeout is real and it fires.
			name:       "explicit-header-timeout-kills",
			readHeader: explicit,
			read:       -1, // so only ReadHeaderTimeout can do the killing
			dribble:    5 * explicit,
			wantServed: false,
		},
		{
			// celeris#594: -1 means no header timeout, even though
			// ReadTimeout is enabled and short — and even though 10s is
			// what the default would have been.
			name:       "sentinel-disables",
			readHeader: -1,
			read:       explicit,
			dribble:    11 * time.Second, // > the 10s ReadHeaderTimeout default
			wantServed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dribble := tc.dribble
			addr, stop := startEngineCfg(t, resource.Config{
				ReadTimeout:       tc.read,
				ReadHeaderTimeout: tc.readHeader,
				WriteTimeout:      -1,
				IdleTimeout:       -1,
			}, okHandler())
			defer stop()

			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()

			start := time.Now()
			type result struct {
				elapsed time.Duration
				data    string
				err     error
			}
			done := make(chan result, 1)
			go func() {
				_ = conn.SetReadDeadline(start.Add(dribble + 5*time.Second))
				b, rerr := io.ReadAll(conn)
				done <- result{time.Since(start), string(b), rerr}
			}()

			writeErr := dribbleHeaders(conn, dribble)

			var got result
			select {
			case got = <-done:
			case <-time.After(dribble + 10*time.Second):
				t.Fatalf("client read did not finish within %v", dribble+10*time.Second)
			}

			served := strings.Contains(got.data, "HTTP/1.1 200")
			t.Logf("readHeaderTimeout=%v readTimeout=%v dribble=%v -> served=%v "+
				"firstResultAfter=%v writeErr=%v readErr=%v response=%q",
				tc.readHeader, tc.read, dribble, served, got.elapsed.Round(time.Millisecond),
				writeErr, got.err, firstLine(got.data))

			if served != tc.wantServed {
				t.Fatalf("served=%v, want %v (response %q, writeErr=%v, readErr=%v)",
					served, tc.wantServed, got.data, writeErr, got.err)
			}

			if !tc.wantServed {
				// The kill has to be the timeout, not the end of the
				// dribble: the connection must go away well before the
				// client finishes writing its headers.
				if got.elapsed >= dribble {
					t.Errorf("connection survived %v, i.e. the whole %v dribble: "+
						"ReadHeaderTimeout=%v did not fire", got.elapsed, dribble, tc.readHeader)
				}
				// Measured shape on go1.27: net/http answers the expired
				// header deadline with "HTTP/1.1 400 Bad Request" and
				// hangs up, so the client's own write fails with a broken
				// pipe part-way through the dribble. A bare close is the
				// other legitimate shape (isCommonNetReadError,
				// server.go:2092: "don't reply"), so neither is asserted —
				// only "not a 200, and early" is.
				if writeErr == nil && got.err == nil && got.data == "" {
					t.Errorf("connection produced neither a response nor an error; "+
						"cannot attribute the outcome (elapsed %v)", got.elapsed)
				}
			}
		})
	}
}

// TestIdleTimeoutSentinelOnTheWire is the IdleTimeout half of the same gap:
// net/http's Server.idleTimeout() also falls back to ReadTimeout when
// IdleTimeout is 0 (go1.27 server.go:3745), so a -1 that arrives as 0 turns
// "keep this connection open indefinitely" into "close it after ReadTimeout".
//
// A keep-alive connection is left idle for 4x the enabled ReadTimeout and
// must still serve a second request.
func TestIdleTimeoutSentinelOnTheWire(t *testing.T) {
	const (
		read = 300 * time.Millisecond
		idle = 1200 * time.Millisecond // 4x read
	)

	addr, stop := startEngineCfg(t, resource.Config{
		ReadTimeout:       read, // the value net/http would fall back to
		ReadHeaderTimeout: -1,
		WriteTimeout:      -1,
		IdleTimeout:       -1,
	}, okHandler())
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	const keepAliveReq = "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"
	readResponse := func(what string) string {
		t.Helper()
		if _, werr := conn.Write([]byte(keepAliveReq)); werr != nil {
			t.Fatalf("%s: write: %v", what, werr)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		n, rerr := conn.Read(buf)
		if rerr != nil {
			t.Fatalf("%s: read: %v (idle timeout was disabled; the connection must still be usable)", what, rerr)
		}
		return string(buf[:n])
	}

	if got := readResponse("first request"); !strings.Contains(got, "HTTP/1.1 200") {
		t.Fatalf("first request: got %q, want a 200", firstLine(got))
	}

	time.Sleep(idle)

	got := readResponse("second request after idle")
	t.Logf("idleTimeout=-1 readTimeout=%v idleFor=%v -> second response %q", read, idle, firstLine(got))
	if !strings.Contains(got, "HTTP/1.1 200") {
		t.Fatalf("second request after %v idle: got %q, want a 200", idle, firstLine(got))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\r'); i >= 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
