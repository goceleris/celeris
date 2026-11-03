package redis

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// celeris#1248: NewSentinelClient asked each sentinel for the master address with
// context.Background(), so a sentinel that accepts the connection and never
// answers held it for ever. The query is now bound by DialTimeout.

// newSentinelWithin1248 runs NewSentinelClient and fails the test if it does not
// return: the deadline only bounds a hang (on the unfixed code, for ever).
func newSentinelWithin1248(t *testing.T, cfg SentinelConfig) (*SentinelClient, error) {
	t.Helper()
	type result struct {
		sc  *SentinelClient
		err error
	}
	done := make(chan result, 1)
	go func() {
		sc, err := NewSentinelClient(cfg)
		done <- result{sc, err}
	}()
	select {
	case r := <-done:
		return r.sc, r.err
	case <-time.After(30 * time.Second):
		t.Fatal("NewSentinelClient did not return")
		return nil, nil
	}
}

// The only sentinel accepts and never answers: NewSentinelClient returns an
// error after DialTimeout, and the Client built for the query is closed once.
func TestNewSentinelClientFailsWhenTheOnlySentinelNeverAnswers1248(t *testing.T) {
	for name, srv := range dialModes1246(t) {
		t.Run(name, func(t *testing.T) {
			hang := startHangServer1246(t)
			before := eventloop.StandaloneRefs()
			sc, err := newSentinelWithin1248(t, SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{hang.ln.Addr().String()},
				DialTimeout:   200 * time.Millisecond,
				Engine:        srv,
			})
			if err == nil {
				_ = sc.Close()
				t.Fatal("NewSentinelClient succeeded against a sentinel that never answers")
			}
			if hang.accepted.Load() == 0 {
				t.Error("the sentinel was never contacted")
			}
			if !strings.Contains(err.Error(), "no responsive sentinel") || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want a discovery failure that wraps context.DeadlineExceeded", err)
			}
			wantRefs(t, before, "after the failed discovery")
		})
	}
}

// The first sentinel never answers and the second does: the discovery moves on to
// the second.
func TestNewSentinelClientMovesOnFromASentinelThatNeverAnswers1248(t *testing.T) {
	for name, srv := range dialModes1246(t) {
		t.Run(name, func(t *testing.T) {
			master := startFakeMaster(t)
			good := startFakeSentinel(t, master.Addr())
			hang := startHangServer1246(t)
			before := eventloop.StandaloneRefs()
			sc, err := newSentinelWithin1248(t, SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{hang.ln.Addr().String(), good.Addr()},
				DialTimeout:   200 * time.Millisecond,
				Engine:        srv,
			})
			if err != nil {
				t.Fatalf("NewSentinelClient: %v", err)
			}
			if hang.accepted.Load() == 0 {
				t.Error("the first sentinel was never contacted")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := sc.Set(ctx, "k", "v", 0); err != nil {
				t.Errorf("Set on the discovered master: %v", err)
			}
			closeWithin1246(t, sc, "the Close of the SentinelClient")
			wantRefs(t, before, "closed")
		})
	}
}

// The master the sentinel names answers the handshake and never answers ROLE: the
// ROLE reply is bound by DialTimeout too (without the bound, nothing ends the
// dial), the construction fails, and the master's Client is closed once.
func TestNewSentinelClientFailsWhenTheMasterNeverAnswersROLE1248(t *testing.T) {
	for name, srv := range dialModes1246(t) {
		t.Run(name, func(t *testing.T) {
			var roles atomic.Int32
			master := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
				if len(cmd) == 0 {
					return
				}
				switch strings.ToUpper(cmd[0]) {
				case "HELLO":
					handleHELLO(w, 3)
				case "ROLE":
					roles.Add(1) // never answered
				default:
					writeSimple(w, "OK")
				}
			})
			sentinel := startFakeSentinel(t, master.Addr())
			before := eventloop.StandaloneRefs()
			sc, err := newSentinelWithin1248(t, SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{sentinel.Addr()},
				DialTimeout:   200 * time.Millisecond,
				Engine:        srv,
			})
			if err == nil {
				_ = sc.Close()
				t.Fatal("NewSentinelClient succeeded with a master that never answers ROLE")
			}
			if roles.Load() == 0 {
				t.Error("the master never received ROLE: the handshake bound ended the dial, not the ROLE one")
			}
			if !strings.Contains(err.Error(), "ROLE failed") || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want a ROLE failure that wraps context.DeadlineExceeded", err)
			}
			wantRefs(t, before, "after the failed ROLE of the master")
		})
	}
}

// The error of a failed discovery holds the error of every sentinel that was
// tried: the first answers "not found" and the second never answers, and neither
// is lost.
func TestNewSentinelClientKeepsTheErrorOfEverySentinel1248(t *testing.T) {
	for name, srv := range dialModes1246(t) {
		t.Run(name, func(t *testing.T) {
			noMaster := startFakeSentinel(t, "")
			hang := startHangServer1246(t)
			before := eventloop.StandaloneRefs()
			sc, err := newSentinelWithin1248(t, SentinelConfig{
				MasterName:    "mymaster",
				SentinelAddrs: []string{noMaster.Addr(), hang.ln.Addr().String()},
				DialTimeout:   200 * time.Millisecond,
				Engine:        srv,
			})
			if err == nil {
				_ = sc.Close()
				t.Fatal("NewSentinelClient succeeded with no master")
			}
			msg := err.Error()
			for _, want := range []string{"no responsive sentinel", `master "mymaster" not found`, hang.ln.Addr().String()} {
				if !strings.Contains(msg, want) {
					t.Errorf("err = %q, want it to mention %q", msg, want)
				}
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want it to wrap context.DeadlineExceeded (the sentinel that never answers)", err)
			}
			wantRefs(t, before, "after the failed discovery")
		})
	}
}
