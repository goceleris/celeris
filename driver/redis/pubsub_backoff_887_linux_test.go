//go:build linux

package redis

// celeris#887 item 30: the reconnect loop reset its backoff as soon as the
// replay of the subscriptions returned, so a server that hangs up just after
// the replay was retried at the base delay for ever (the loop's own in-loop
// check covers only a conn that dies during the replay, item 15). The backoff
// must start over only after the replayed conn has stayed up for a while
// (pubsubStableAfter).

import (
	"bufio"
	"context"
	"sync"
	"testing"
	"time"
)

// hangUpServer is a fake server that answers SUBSCRIBE and then closes the
// conn after lifetime(n), n counting the SUBSCRIBEs it has seen from 1 (0
// keeps the conn). It records when each SUBSCRIBE arrived.
type hangUpServer struct {
	fake     *fakeRedis
	lifetime func(n int) time.Duration
	mu       sync.Mutex
	stamps   []time.Time
}

func startHangUpServer(t *testing.T, lifetime func(n int) time.Duration) *hangUpServer {
	t.Helper()
	s := &hangUpServer{lifetime: lifetime}
	ready := make(chan struct{})
	s.fake = startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		<-ready
		if len(cmd) == 0 || cmd[0] != "SUBSCRIBE" {
			writeSimple(w, "OK")
			return
		}
		writeArrayHeader(w, 3)
		writeBulk(w, "subscribe")
		writeBulk(w, cmd[1])
		writeInt(w, 1)
		s.mu.Lock()
		s.stamps = append(s.stamps, time.Now())
		n := len(s.stamps)
		s.mu.Unlock()
		if d := s.lifetime(n); d > 0 {
			s.fake.mu.Lock()
			c := s.fake.conns[len(s.fake.conns)-1]
			s.fake.mu.Unlock()
			time.AfterFunc(d, func() { _ = c.Close() })
		}
	})
	close(ready)
	return s
}

func (s *hangUpServer) times() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.stamps...)
}

func subscribeToHangUpServer(t *testing.T, s *hangUpServer) *PubSub {
	t.Helper()
	cl, err := NewClient(s.fake.Addr(), WithForceRESP2())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ps, err := cl.Subscribe(ctx, "news")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	return ps
}

// The server hangs up 10 ms after every SUBSCRIBE. The attempts must back off:
// the base delay is 50 ms and doubles up to 5 s, so in 2.5 s the server sees
// about seven SUBSCRIBEs (50+100+200+400+800+1600 ms of delay), not the fifty
// of a loop that starts over from 50 ms every time.
func TestReconnectBacksOffWhenTheServerHangsUpAfterEveryReplay887(t *testing.T) {
	s := startHangUpServer(t, func(int) time.Duration { return 10 * time.Millisecond })
	subscribeToHangUpServer(t, s)
	time.Sleep(2500 * time.Millisecond)
	ts := s.times()
	for i := 1; i < len(ts); i++ {
		t.Logf("SUBSCRIBE %d came %v after the previous one", i+1, ts[i].Sub(ts[i-1]).Round(time.Millisecond))
	}
	if len(ts) > 12 {
		t.Errorf("the server saw %d SUBSCRIBEs in 2.5 s: the reconnect backoff started over after every replay", len(ts))
	}
	if n := len(ts); n >= 5 {
		if gap := ts[n-1].Sub(ts[n-2]); gap < 250*time.Millisecond {
			t.Errorf("the gap before SUBSCRIBE %d is %v, want >= 250 ms: the delay did not grow", n, gap)
		}
	}
}

// A conn that stays up for pubsubStableAfter ends the outage: the next drop is
// retried from the base delay again.
func TestReconnectBackoffStartsOverAfterTheConnWasStable887(t *testing.T) {
	old := pubsubStableAfter
	pubsubStableAfter = 400 * time.Millisecond
	t.Cleanup(func() { pubsubStableAfter = old })

	// SUBSCRIBEs 1..5 are cut after 10 ms; the sixth lives 800 ms (longer than
	// the stable window), then everything is cut after 10 ms again.
	s := startHangUpServer(t, func(n int) time.Duration {
		if n == 6 {
			return 800 * time.Millisecond
		}
		return 10 * time.Millisecond
	})
	subscribeToHangUpServer(t, s)
	waitFor(t, "the seventh SUBSCRIBE", 20*time.Second, func() bool { return len(s.times()) >= 7 })
	ts := s.times()
	for i := 1; i < len(ts); i++ {
		t.Logf("SUBSCRIBE %d came %v after the previous one", i+1, ts[i].Sub(ts[i-1]).Round(time.Millisecond))
	}
	// Before the stable conn the attempts back off (the fifth reconnect waits
	// about 800 ms) ...
	if gap := ts[4].Sub(ts[3]); gap < 250*time.Millisecond {
		t.Errorf("the gap before SUBSCRIBE 5 is %v, want >= 250 ms: the delay did not grow", gap)
	}
	// ... and after the conn that lived 800 ms the next reconnect is at the
	// base delay again.
	gap := ts[6].Sub(ts[5]) - 800*time.Millisecond
	if gap > 250*time.Millisecond {
		t.Errorf("the reconnect after a stable conn waited %v, want about the base delay (50 ms): the backoff did not start over", gap)
	}
}
