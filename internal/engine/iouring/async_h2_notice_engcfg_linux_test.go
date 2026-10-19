//go:build linux

package iouring

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// The io_uring twin of epoll's async_h2_notice_engcfg_linux_test.go: the note
// that AsyncHandlers does not reach HTTP/2 must be logged whenever HTTP/2 is
// reachable (checklist #884 item 44, planned as 43), which is not "when
// EnableH2Upgrade" (Auto and H2C take prior-knowledge h2c with the upgrade
// off) and not "when Protocol != HTTP1" (HTTP1 with the upgrade on upgrades).
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

// asyncNoticeRecords starts an io_uring engine with cfg, stops it, and returns
// the messages it logged that name AsyncHandlers. The log is read after Listen
// returned, so a row that expects no note cannot pass before the line was
// written. The start retries the ring ENOMEM a runner's 8 MiB memlock gives
// when engines start back to back (startRingRetried662); two workers are
// asked for, and the engine caps that to what memlock funds.
func asyncNoticeRecords(t *testing.T, cfg resource.Config) []string {
	t.Helper()
	var log lockedBuf
	cfg.Addr = "127.0.0.1:0"
	cfg.Engine = celerisengine.IOUring
	cfg.Resources = resource.Resources{Workers: 2} // the engines refuse fewer
	cfg.Logger = slog.New(slog.NewJSONHandler(&log, nil))
	_, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(cfg, respondingHandler{})
	})
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
		if strings.HasPrefix(rec.Msg, "io_uring engine listening") {
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
