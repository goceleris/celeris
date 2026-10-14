//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"

	"golang.org/x/net/http2"
)

// TestEnableH2UpgradeMatrix889 drives celeris#889 (item 14) on every engine
// over a real connection, through celeris.Config's *bool: what an
// Upgrade: h2c request and an HTTP/2 preface get for every Protocol and every
// EnableH2Upgrade value. The field's documentation is the oracle, and the
// native engines the reference:
//
//	Protocol  prior-knowledge h2c  Upgrade: h2c
//	Auto      yes                  unless &false
//	H2C       yes                  only &true
//	HTTP1     no                   only &true
//
// std used to upgrade on H2C for nil and &false as well and never on HTTP1.
//
// An upgrade request that is not honoured is, on Auto and HTTP1, served as
// the HTTP/1.1 request it also is, on every engine. On H2C the native engines
// treat the whole listener as HTTP/2 and answer it with their SETTINGS frame;
// std keeps serving plain HTTP/1.1 on an H2C listener (it always did:
// TestStdEngineStillServesPlainHTTP1OnAnH2CListener), so there it is served as
// HTTP/1.1. That one difference in the first reply is the only one left.
func TestEnableH2UpgradeMatrix889(t *testing.T) {
	protocols := []struct {
		name           string
		p              celeris.Protocol
		priorKnowledge bool
		upgrade        map[string]bool // by EnableH2Upgrade: nil, false, true
	}{
		{"Auto", celeris.Auto, true, map[string]bool{"nil": true, "false": false, "true": true}},
		{"H2C", celeris.H2C, true, map[string]bool{"nil": false, "false": false, "true": true}},
		{"HTTP1", celeris.HTTP1, false, map[string]bool{"nil": false, "false": false, "true": true}},
	}
	enables := []struct {
		name string
		v    *bool
	}{{"nil", nil}, {"false", bptr964(false)}, {"true", bptr964(true)}}

	var preface bytes.Buffer
	preface.WriteString(http2.ClientPreface)
	if err := http2.NewFramer(&preface, nil).WriteSettings(); err != nil {
		t.Fatal(err)
	}

	for _, e := range engines761 {
		for _, pr := range protocols {
			for _, en := range enables {
				t.Run(e.name+"/"+pr.name+"/"+en.name, func(t *testing.T) {
					cfg := celeris.Config{Engine: e.eng, Protocol: pr.p, EnableH2Upgrade: en.v}

					// The upgrade request.
					got := firstReply889(t, startTCPReady889(t, cfg), upgradeRequest964)
					want := "http1-200"
					switch {
					case pr.upgrade[en.name]:
						want = "101"
					case pr.p == celeris.H2C && e.name != "std":
						want = "h2-settings"
					}
					t.Logf("FIRST engine=%s protocol=%s EnableH2Upgrade=%s request=upgrade -> %s %q", e.name, pr.name, en.name, replyKind889(got), got)
					if kind := replyKind889(got); kind != want {
						t.Errorf("Upgrade: h2c request: first reply %q (%s), want %s", got, kind, want)
					}

					// The HTTP/2 preface: served as HTTP/2 where the protocol has
					// prior-knowledge h2c, whatever EnableH2Upgrade says, and not
					// otherwise.
					got = firstReply889(t, startTCPReady889(t, cfg), preface.String())
					t.Logf("FIRST engine=%s protocol=%s EnableH2Upgrade=%s request=preface -> %s %q", e.name, pr.name, en.name, replyKind889(got), got)
					if kind := replyKind889(got); pr.priorKnowledge != (kind == "h2-settings") {
						t.Errorf("HTTP/2 preface: first reply %q (%s), prior-knowledge h2c served = %v, want %v",
							got, kind, kind == "h2-settings", pr.priorKnowledge)
					}
				})
			}
		}
	}
}

// TestUpgradeNeedsTheUpgradeConnectionToken889: an Upgrade: h2c request whose
// Connection lists HTTP2-Settings but not Upgrade is not the h2c upgrade on
// any engine (the native parser needs both tokens; std used to ask for
// HTTP2-Settings alone and answered 101). It is served as the HTTP/1.1
// request it also is. The upgrade is on for every row.
func TestUpgradeNeedsTheUpgradeConnectionToken889(t *testing.T) {
	const wire = "GET /h HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Connection: HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n" +
		"\r\n"
	for _, e := range engines761 {
		for _, pr := range []struct {
			name string
			p    celeris.Protocol
		}{{"Auto", celeris.Auto}, {"HTTP1", celeris.HTTP1}} {
			t.Run(e.name+"/"+pr.name, func(t *testing.T) {
				cfg := celeris.Config{Engine: e.eng, Protocol: pr.p, EnableH2Upgrade: bptr964(true)}
				got := firstReply889(t, startTCPReady889(t, cfg), wire)
				t.Logf("FIRST engine=%s protocol=%s request=no-upgrade-token -> %s %q", e.name, pr.name, replyKind889(got), got)
				if kind := replyKind889(got); kind != "http1-200" {
					t.Errorf("Connection: HTTP2-Settings alone: first reply %q (%s), want an HTTP/1.1 200", got, kind)
				}
			})
		}
	}
}

// replyKind889 classifies the first bytes of a reply: "101" (the upgrade),
// "http1-200", "h2-settings" (an HTTP/2 SETTINGS frame header, stream 0),
// "http1-other" (any other HTTP/1.x status line), "closed" (nothing), "other".
func replyKind889(b []byte) string {
	s := string(b)
	switch {
	case strings.HasPrefix(s, "HTTP/1.1 101"):
		return "101"
	case strings.HasPrefix(s, "HTTP/1.1 200"):
		return "http1-200"
	case strings.HasPrefix(s, "HTTP/1."):
		return "http1-other"
	case len(b) >= 9 && b[3] == 0x04 && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0:
		return "h2-settings"
	case len(b) == 0:
		return "closed"
	}
	return "other"
}

// firstReply889 sends wire and returns what the server sent first: up to the
// first CRLF or 16 bytes (an HTTP/2 frame header and the start of its
// payload), or whatever came before the connection ended or 5 s
// passed.
func firstReply889(t *testing.T, addr, wire string) []byte {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, wire); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	var got []byte
	for len(got) < 16 && !bytes.Contains(got, []byte("\r\n")) {
		b, err := br.ReadByte()
		if err != nil {
			break
		}
		got = append(got, b)
	}
	return got
}

// startTCPReady889 starts a server with cfg and returns its address once it
// accepts TCP (an H2C listener need not answer HTTP/1.1, which is how the
// other starters here decide it is ready).
func startTCPReady889(t *testing.T, cfg celeris.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg.Addr = addr
	s := celeris.New(cfg)
	s.GET("/h", func(c *celeris.Context) error { return c.String(http.StatusOK, "h1-answer") })
	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	for i := 0; ; i++ {
		select {
		case err := <-done:
			t.Fatalf("Start returned before the listener was up: %v", err)
		default:
		}
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			break
		}
		if i > 300 {
			t.Fatalf("not listening: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Errorf("Start did not return within 15s of Shutdown")
		}
	})
	return addr
}
