package stream

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#889 item 6 and the "window waits wake every waiter" item (#906, #893):
// a handler that waits in AwaitSendWindow is woken by a frame that can open ITS
// windows, not by every WINDOW_UPDATE of the connection.
//
// The assertions need no hook. A waiter's window is opened without a notice
// (SetWindowSize alone wakes nobody): a waiter that is woken by a frame that
// has nothing to do with its stream finds its windows open and returns, one
// that is not woken stays asleep.

// waiter889 is a goroutine in AwaitSendWindow.
type waiter889 struct {
	s    *Stream
	res  atomic.Int32 // 0 waiting, 1 windows open, 2 context.Canceled, 3 deadline, 4 anything else
	done chan struct{}
}

func startWaiter889(s *Stream, deadline time.Time) *waiter889 {
	w := &waiter889{s: s, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		switch err := s.AwaitSendWindow(deadline); {
		case err == nil:
			w.res.Store(1)
		case errors.Is(err, context.Canceled):
			w.res.Store(2)
		case errors.Is(err, os.ErrDeadlineExceeded):
			w.res.Store(3)
		default:
			w.res.Store(4)
		}
	}()
	return w
}

// waitingStream889 is a pool stream (a handler can wait on it) whose send
// window is closed.
func waitingStream889(t *testing.T, m *Manager, id uint32) *Stream {
	t.Helper()
	s := openStream893(t, m, id)
	s.flags.Or(flagAsyncRunning)
	s.SetWindowSize(0)
	return s
}

func awaitWaiters889(t *testing.T, m *Manager, n int) {
	t.Helper()
	for until := time.Now().Add(5 * time.Second); int(m.sendWindowWaiters.Load()) != n; {
		if time.Now().After(until) {
			t.Fatalf("%d waiters, want %d", m.sendWindowWaiters.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
	// Counted is not asleep: a waiter counts itself, registers, and only then
	// looks at its windows. Give it the time to reach its select.
	time.Sleep(50 * time.Millisecond)
}

func (w *waiter889) wait(t *testing.T, d time.Duration) int32 {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(d):
	}
	return w.res.Load()
}

// A WINDOW_UPDATE for one stream wakes the handler waiting on that stream and
// no other: not the one waiting on another stream, and no one for a stream
// nobody waits on.
func TestStreamWindowUpdateWakesOnlyItsWaiter889(t *testing.T) {
	for _, raw := range []bool{false, true} {
		name := "frame"
		if raw {
			name = "raw"
		}
		t.Run(name, func(t *testing.T) {
			p, m := newBudgetProcessor893(t, nil)
			a := waitingStream889(t, m, 1)
			b := waitingStream889(t, m, 3)
			other := openStream893(t, m, 5) // nobody waits on it
			wa := startWaiter889(a, time.Now().Add(time.Minute))
			wb := startWaiter889(b, time.Now().Add(time.Minute))
			awaitWaiters889(t, m, 2)

			// b's window is open now, and nothing said so.
			b.SetWindowSize(100)

			update := func(id uint32) {
				if raw {
					if err := p.HandleRawWindowUpdate(id, []byte{0, 0, 0, 10}); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := p.ProcessFrame(context.Background(), makeWindowUpdateFrame(t, id, 10)); err != nil {
					t.Fatal(err)
				}
			}
			update(other.ID) // a stream nobody waits on
			update(a.ID)     // a's own: a wakes, finds 10 bytes of window and the connection's window open

			if r := wa.wait(t, 5*time.Second); r != 1 {
				t.Fatalf("the handler waiting on stream 1 was not woken by stream 1's WINDOW_UPDATE (result %d)", r)
			}
			if r := wb.wait(t, 300*time.Millisecond); r != 0 {
				t.Fatalf("the handler waiting on stream 3 returned (%d) on WINDOW_UPDATEs for streams 5 and 1: "+
					"every waiter of the connection is woken by every WINDOW_UPDATE", r)
			}

			// Its own frame does wake it.
			update(b.ID)
			if r := wb.wait(t, 5*time.Second); r != 1 {
				t.Fatalf("the handler waiting on stream 3 was not woken by stream 3's WINDOW_UPDATE (result %d)", r)
			}
			m.Close()
		})
	}
}

// The deadline of one wait wakes that waiter and no other: N waits with
// deadlines close together cost N wakeups, not N x N (item 6).
func TestDeadlineOfOneWaitDoesNotWakeTheOthers889(t *testing.T) {
	_, m := newBudgetProcessor893(t, nil)
	a := waitingStream889(t, m, 1)
	b := waitingStream889(t, m, 3)
	wa := startWaiter889(a, time.Now().Add(80*time.Millisecond))
	wb := startWaiter889(b, time.Now().Add(time.Minute))
	awaitWaiters889(t, m, 2)
	b.SetWindowSize(100) // open, and nothing said so

	if r := wa.wait(t, 5*time.Second); r != 3 {
		t.Fatalf("the wait with the short deadline ended with %d, want the deadline (3)", r)
	}
	if r := wb.wait(t, 300*time.Millisecond); r != 0 {
		t.Fatalf("the handler waiting on stream 3 returned (%d) when another wait's deadline passed: "+
			"the deadline's timer wakes every waiter of the connection", r)
	}
	m.Close()
	if r := wb.wait(t, 5*time.Second); r != 2 {
		t.Fatalf("the waiter did not end with the connection (result %d)", r)
	}
}

// A connection WINDOW_UPDATE ends the wait of a handler whose stream window is
// open and whose connection window was the bottleneck, and a SETTINGS change of
// the initial window ends that of one whose stream window it opens.
func TestConnectionWindowUpdateWakesTheWaitersItOpens889(t *testing.T) {
	p, m := newBudgetProcessor893(t, nil)
	a := waitingStream889(t, m, 1)
	a.SetWindowSize(1000) // open: the connection window is what it waits for
	m.connectionWindow = 0
	wa := startWaiter889(a, time.Now().Add(time.Minute))
	awaitWaiters889(t, m, 1)
	if r := wa.wait(t, 100*time.Millisecond); r != 0 {
		t.Fatalf("returned (%d) with the connection window closed", r)
	}
	if err := p.HandleRawWindowUpdate(0, []byte{0, 0, 0, 10}); err != nil {
		t.Fatal(err)
	}
	if r := wa.wait(t, 5*time.Second); r != 1 {
		t.Fatalf("a connection WINDOW_UPDATE did not wake the waiter whose stream window is open (result %d)", r)
	}
	m.Close()
}

// The lost-wakeup check: waiters that register while WINDOW_UPDATEs arrive, in
// every order the scheduler gives, all end. Windows only grow here, so a waiter
// that does not end was not woken for a window that opened after it looked.
func TestWindowWaitersAreNeverLost889(t *testing.T) {
	const streams = 24
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	for round := range rounds {
		p, m := newBudgetProcessor893(t, nil)
		m.connectionWindow = 0
		ws := make([]*waiter889, streams)
		ss := make([]*Stream, streams)
		for i := range ss {
			ss[i] = waitingStream889(t, m, uint32(2*i+1))
		}
		var wg sync.WaitGroup
		// Grants: the connection window, then each stream's, in a random order,
		// while the waiters start.
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(round), 1))
			order := rng.Perm(streams + 1)
			for _, k := range order {
				id := uint32(0)
				if k < streams {
					id = ss[k].ID
				}
				if err := p.HandleRawWindowUpdate(id, []byte{0, 0, 0, 1}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		for i := range ws {
			ws[i] = startWaiter889(ss[i], time.Now().Add(time.Minute))
		}
		wg.Wait()
		for i, w := range ws {
			if r := w.wait(t, 5*time.Second); r != 1 {
				t.Fatalf("round %d: the waiter on stream %d did not end (result %d) after every window opened", round, ss[i].ID, r)
			}
		}
		m.Close()
	}
}

// The regime of a client that reads: its stream windows are open and the
// connection window is the bottleneck, so each waiter wakes once per
// connection WINDOW_UPDATE that gives it room, takes the room, and waits
// again. Every credit is taken by some waiter, so all of them end.
func TestConnectionWindowRaceWaitersEnd889(t *testing.T) {
	const (
		streams = 12
		perWait = 20
	)
	p, m := newBudgetProcessor893(t, nil)
	m.connectionWindow = 0
	var wg sync.WaitGroup
	for i := range streams {
		s := waitingStream889(t, m, uint32(2*i+1))
		s.SetWindowSize(1 << 30)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for taken := 0; taken < perWait; {
				if err := s.AwaitSendWindow(time.Now().Add(20 * time.Second)); err != nil {
					t.Errorf("stream %d: %v after %d credits", s.ID, err, taken)
					return
				}
				for { // take one unit of the connection window, if any is left
					w := atomic.LoadInt32(&m.connectionWindow)
					if w <= 0 {
						break
					}
					if atomic.CompareAndSwapInt32(&m.connectionWindow, w, w-1) {
						taken++
						break
					}
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for i := 0; i < streams*perWait; i++ {
		if err := p.HandleRawWindowUpdate(0, []byte{0, 0, 0, 1}); err != nil {
			t.Fatal(err)
		}
		if i%7 == 0 {
			time.Sleep(50 * time.Microsecond)
		}
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("waiters did not take %d credits: a connection WINDOW_UPDATE was not seen by a waiter it opened", streams*perWait)
	}
	m.Close()
}

// Quiescent lost-wakeup check with real writers. Each writer waits for its
// windows and reserves from them with ReserveSendWindow, which dips a stream
// window to 0 for an instant (it takes the stream's credit before it knows the
// connection's, and refunds the overage), while the event loop grants windows.
// The grants are a FIXED, shuffled list that sums to exactly what the writers
// need; after the last one a writer that still owes bytes while both its
// windows are open was left asleep by a wakeup that was skipped or lost: no
// later grant exists to cure it (a test that keeps granting until the writers
// finish cannot tell, the next grant wakes the sleeper). writersPerStream 2 is
// outside the StreamWriter contract (one writer per stream), and is the case
// that needs the refund's notice. Race checks are probabilistic: with every
// connection-frame wakeup disabled this reports tens of lost waiters.
func TestRealWritersAreNeverLeftAsleep889(t *testing.T) {
	for _, writersPerStream := range []int{1, 2} {
		t.Run(string(rune('0'+writersPerStream))+"-writers-per-stream", func(t *testing.T) {
			rounds := 40
			if testing.Short() {
				rounds = 8
			}
			for round := range rounds {
				p, m := newBudgetProcessor893(t, nil)
				m.connectionWindow = 0
				const streams, need, chunk = 6, 3000, 100
				ss := make([]*Stream, streams)
				for i := range ss {
					ss[i] = waitingStream889(t, m, uint32(2*i+1))
				}
				var mu sync.Mutex
				remaining := map[*Stream]int{}
				for _, s := range ss {
					remaining[s] = need * writersPerStream
				}
				var wg sync.WaitGroup
				for _, s := range ss {
					for range writersPerStream {
						wg.Add(1)
						go func() {
							defer wg.Done()
							for {
								mu.Lock()
								r := remaining[s]
								mu.Unlock()
								if r <= 0 {
									return
								}
								if err := s.AwaitSendWindow(time.Time{}); err != nil {
									return // the connection was closed
								}
								got := m.ReserveSendWindow(s, int32(min(r, 300)))
								mu.Lock()
								remaining[s] -= int(got)
								mu.Unlock()
							}
						}()
					}
				}
				var grants []uint32 // per stream need*writers in chunks, and the connection's sum
				for _, s := range ss {
					for range need * writersPerStream / chunk {
						grants = append(grants, s.ID, 0)
					}
				}
				rng := rand.New(rand.NewPCG(uint64(round), uint64(writersPerStream)))
				rng.Shuffle(len(grants), func(i, j int) { grants[i], grants[j] = grants[j], grants[i] })
				for i, id := range grants {
					if err := p.HandleRawWindowUpdate(id, []byte{0, 0, 0, chunk}); err != nil {
						t.Fatal(err)
					}
					if i%16 == 0 {
						time.Sleep(time.Duration(rng.IntN(20)) * time.Microsecond)
					}
				}
				owed := func() (n int) {
					mu.Lock()
					defer mu.Unlock()
					for _, r := range remaining {
						if r > 0 {
							n++
						}
					}
					return n
				}
				// The quiet period: the grants are all in, nothing else will wake anyone.
				for until := time.Now().Add(3 * time.Second); owed() > 0 && time.Now().Before(until); {
					time.Sleep(time.Millisecond)
				}
				for _, s := range ss {
					mu.Lock()
					r := remaining[s]
					mu.Unlock()
					if r > 0 && m.WindowsOpen(s) {
						t.Errorf("round %d: a writer of stream %d is asleep with both windows open (owes %d, stream window %d, connection window %d)",
							round, s.ID, r, s.windowSize.Load(), atomic.LoadInt32(&m.connectionWindow))
					}
				}
				m.Close()
				wg.Wait()
			}
		})
	}
}
