//go:build linux

package epoll

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// The start-up note that says AsyncHandlers does not reach HTTP/2 (checklist
// #885 item 26 here, its io_uring twin #884 item 44 (planned as 43)) was
// printed only when AsyncHandlers && EnableH2Upgrade. HTTP/2 is reachable
// without the upgrade: Protocol Auto and H2C accept prior-knowledge h2c, and
// the h2c conn then runs its handlers by the rules the note describes. The
// checklist proposed gating on Protocol != HTTP1, which drops the other half:
// Protocol HTTP1 with EnableH2Upgrade=true still upgrades to HTTP/2.
//
// Each row runs a real engine to its "listening" line, stops it, and reads
// the log after Listen returned, so a row that expects no note cannot pass
// before the line was written. EnableH2Upgrade is set through SetH2Upgrade,
// as celeris.Config does: a plain field set to false on Auto is turned back
// into true by WithDefaults and would hide the cases the old gate gets wrong.
type asyncNoticeRow struct {
	name    string
	proto   celerisengine.Protocol
	upgrade bool
	async   bool
	want    bool
}

var asyncNoticeRows = []asyncNoticeRow{
	{"auto-upgrade-off", celerisengine.Auto, false, true, true},
	{"h2c-upgrade-off", celerisengine.H2C, false, true, true},
	{"auto-upgrade-on", celerisengine.Auto, true, true, true},
	{"h2c-upgrade-on", celerisengine.H2C, true, true, true},
	{"http1-upgrade-on", celerisengine.HTTP1, true, true, true},
	{"http1-upgrade-off", celerisengine.HTTP1, false, true, false},
	{"auto-upgrade-off-sync", celerisengine.Auto, false, false, false},
	{"http1-upgrade-on-sync", celerisengine.HTTP1, true, false, false},
}

// asyncNoticeRecords starts an epoll engine with cfg, stops it, and returns
// the messages it logged that name AsyncHandlers.
func asyncNoticeRecords(t *testing.T, cfg resource.Config) []string {
	t.Helper()
	var log lockedBuf
	cfg.Addr = "127.0.0.1:0"
	cfg.Engine = celerisengine.Epoll
	cfg.Resources = resource.Resources{Workers: 2}
	cfg.Logger = slog.New(slog.NewJSONHandler(&log, nil))
	e, err := New(cfg, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(5 * time.Second); e.Addr() == nil; {
		select {
		case err := <-done:
			t.Fatalf("Listen returned before publishing an address: %v", err)
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(dl) {
			cancel()
			<-done
			t.Fatal("the engine never published an address")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Listen did not return within 30 s of its context being cancelled")
	}
	var msgs []string
	listening := false
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		var rec struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if strings.HasPrefix(rec.Msg, "epoll engine listening") {
			listening = true
		}
		if strings.Contains(rec.Msg, "AsyncHandlers") {
			msgs = append(msgs, rec.Msg)
		}
	}
	if !listening {
		t.Fatalf("the engine never logged its listening line; log:\n%s", log.String())
	}
	return msgs
}

func TestAsyncHandlersH2NoticeIsGatedOnH2BeingReachable(t *testing.T) {
	for _, tc := range asyncNoticeRows {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resource.Config{Protocol: tc.proto, AsyncHandlers: tc.async}
			cfg.SetH2Upgrade(tc.upgrade)
			msgs := asyncNoticeRecords(t, cfg)
			if got := len(msgs) > 0; got != tc.want {
				t.Fatalf("Protocol %v, EnableH2Upgrade %v, AsyncHandlers %v: AsyncHandlers notice logged = %v (%q), want %v",
					tc.proto, tc.upgrade, tc.async, got, msgs, tc.want)
			}
			if len(msgs) > 1 {
				t.Fatalf("the notice was logged %d times: %q", len(msgs), msgs)
			}
			if len(msgs) == 1 && msgs[0] != resource.AsyncH2Notice {
				t.Fatalf("the notice reads %q, want %q", msgs[0], resource.AsyncH2Notice)
			}
		})
	}
}
