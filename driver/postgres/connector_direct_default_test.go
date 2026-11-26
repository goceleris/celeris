package postgres

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
)

func newFakeConnector(t *testing.T, query string) *Connector {
	t.Helper()
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 42, 0x1ABCD123, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
	})
	host, port, _ := net.SplitHostPort(addr)
	cn, err := NewConnector("postgres://u@" + host + ":" + port + "/d?sslmode=disable" + query)
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	t.Cleanup(func() { _ = cn.Close() })
	return cn
}

func connectPG(t *testing.T, cn *Connector) *pgConn {
	t.Helper()
	dc, err := cn.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	c := dc.(*pgConn)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A Connector dials direct connections by default: Go's netpoll, nothing
// registered on a loop, and no loop resolved (a standalone one is not started).
func TestConnectorDialsDirectByDefault(t *testing.T) {
	cn := newFakeConnector(t, "")
	c := connectPG(t, cn)
	if !c.useDirect || c.tcp == nil {
		t.Fatalf("not a direct connection: useDirect=%v tcp=%v", c.useDirect, c.tcp)
	}
	if c.loop != nil || c.syncLoop != nil || c.fd != 0 {
		t.Fatalf("a direct connection is registered on a loop: loop=%v syncLoop=%v fd=%d", c.loop, c.syncLoop, c.fd)
	}
	cn.provMu.Lock()
	prov, owns := cn.provider, cn.ownsLoop
	cn.provMu.Unlock()
	if prov != nil || owns {
		t.Fatal("Connect on a default Connector resolved an event loop")
	}
	// WithEngine alone does not move it onto the loop.
	if c := connectPG(t, cn.WithEngine(nil)); !c.useDirect {
		t.Fatal("WithEngine(nil) put the connection on a loop")
	}
}

// WithStreaming and streaming=true in the DSN put the connections on the loop;
// the Connector it was called on is unchanged, and the option holds in every
// order of the chain.
func TestConnectorWithStreamingUsesTheLoop(t *testing.T) {
	base := newFakeConnector(t, "")
	for _, tc := range []struct {
		name  string
		build func() *Connector
	}{
		{"WithStreaming", func() *Connector { return base.WithStreaming() }},
		{"streaming, then engine", func() *Connector { return base.WithStreaming().WithEngine(nil) }},
		{"engine, then streaming", func() *Connector { return base.WithEngine(nil).WithStreaming() }},
		{"streaming, then busy poll", func() *Connector { return base.WithStreaming().WithSocketBusyPoll(29000) }},
		{"busy poll, then streaming", func() *Connector { return base.WithSocketBusyPoll(29000).WithStreaming() }},
		{"streaming, then max result", func() *Connector { return base.WithStreaming().WithMaxResultBytes(1 << 20) }},
		{"DSN streaming=true", func() *Connector { return newFakeConnector(t, "&streaming=true") }},
		{"DSN streaming=1, then engine", func() *Connector { return newFakeConnector(t, "&streaming=1").WithEngine(nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cn := tc.build()
			t.Cleanup(func() { _ = cn.Close() })
			c := connectPG(t, cn)
			if c.useDirect || c.loop == nil {
				t.Fatalf("not on the loop: useDirect=%v loop=%v", c.useDirect, c.loop)
			}
		})
	}
	if c := connectPG(t, base); !c.useDirect {
		t.Fatal("WithStreaming changed the Connector it was called on")
	}
	if c := connectPG(t, newFakeConnector(t, "&streaming=false")); !c.useDirect {
		t.Fatal("streaming=false is not direct")
	}
}

// The streaming DSN parameter takes a bool and nothing else; it is not passed to
// the server as a startup parameter.
func TestStreamingDSNParameter(t *testing.T) {
	for _, v := range []string{"maybe", "", "2"} {
		if _, err := NewConnector("postgres://u@h/d?sslmode=disable&streaming=" + v); err == nil || !strings.Contains(err.Error(), "streaming") {
			t.Errorf("streaming=%q: err = %v, want an error that names streaming", v, err)
		}
	}
	d, err := ParseDSN("host=h user=u dbname=d streaming=on")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Options.streaming {
		t.Error("key=value form: streaming=on not read")
	}
	if _, ok := d.Params["streaming"]; ok {
		t.Error("streaming was kept as a startup parameter")
	}
}

// The result cap is a setting: set on the connection it reaches the request; a
// value below 1 is an error, not "no cap", from Connect and from Open.
func TestMaxResultBytesSetting(t *testing.T) {
	cn := newFakeConnector(t, "")
	if c := connectPG(t, cn); c.opts.maxResultBytes != 0 {
		t.Fatalf("default maxResultBytes = %d, want 0 (the 64 MiB default)", c.opts.maxResultBytes)
	}
	c := connectPG(t, cn.WithMaxResultBytes(1234))
	if c.opts.maxResultBytes != 1234 {
		t.Fatalf("maxResultBytes = %d, want 1234", c.opts.maxResultBytes)
	}
	req := &pgRequest{doneCh: make(chan struct{})}
	c.enqueue(req)
	if req.maxBytes != 1234 {
		t.Fatalf("request cap = %d, want 1234", req.maxBytes)
	}
	for _, n := range []int{0, -1} {
		dc, err := cn.WithMaxResultBytes(n).Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), "WithMaxResultBytes") {
			if dc != nil {
				_ = dc.Close()
			}
			t.Errorf("Connector.WithMaxResultBytes(%d): err = %v, want an error that names it", n, err)
		}
		if _, err := Open("postgres://u@127.0.0.1:1/d?sslmode=disable", WithMaxResultBytes(n)); err == nil || !strings.Contains(err.Error(), "WithMaxResultBytes") {
			t.Errorf("Open(WithMaxResultBytes(%d)): err = %v, want an error that names it", n, err)
		}
	}
	// The cap holds through the clones.
	cc := cn.WithMaxResultBytes(99).WithEngine(nil).WithSocketBusyPoll(1000).WithStreaming()
	if cc.maxResultBytes != 99 || !cc.maxResultSet {
		t.Errorf("the cap was lost along the chain: %d, %v", cc.maxResultBytes, cc.maxResultSet)
	}
}

// ErrResultTooBig names the settings by their real names.
func TestErrResultTooBigNamesTheSettings(t *testing.T) {
	for _, want := range []string{"WithMaxResultBytes", "Connector.WithStreaming", "paginate", "64 MiB"} {
		if !strings.Contains(ErrResultTooBig.Error(), want) {
			t.Errorf("ErrResultTooBig %q does not name %q", ErrResultTooBig, want)
		}
	}
}
