package eventloop

// celeris#887 item 19: the portable worker's Write held the conn's write lock
// until the kernel took the data, with no deadline, so a peer that stopped
// reading stalled the writer for good (and, from an onRecv that writes, the
// reader goroutine too) until the conn was closed. Every Write now carries a
// write deadline, 30 s by default.

import (
	"net"
	"sync"
	"testing"
	"time"
)

// c887Conn wraps a net.Conn and keeps the write deadline in force at the
// moment of each Write.
type c887Conn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	atWrite  []time.Time
}

func (c *c887Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *c887Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.atWrite = append(c.atWrite, c.deadline)
	c.mu.Unlock()
	return c.Conn.Write(b)
}

func (c *c887Conn) writes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.atWrite...)
}

func TestPortableWriteCarriesTheDefaultWriteDeadline887(t *testing.T) {
	l := newPortableTestLoop(t, 1)
	srv := newLoopbackServer(t)
	client, server := srv.dial(t)
	wc := &c887Conn{Conn: client}
	rec := newRecorder()
	id, err := registrar(t, l, 0).RegisterNetConn(wc, rec.onRecv, rec.onClose)
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	before := time.Now()
	if err := l.WorkerLoop(0).Write(id, []byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := time.Now()
	buf := make([]byte, 4)
	_ = server.SetReadDeadline(time.Now().Add(portableWait))
	if _, err := server.Read(buf); err != nil {
		t.Fatalf("server read: %v", err)
	}
	ws := wc.writes()
	if len(ws) != 1 {
		t.Fatalf("the conn saw %d Writes, want 1", len(ws))
	}
	d := ws[0]
	if d.IsZero() {
		t.Fatal("the Write was issued with no write deadline: a peer that stops reading stalls it for good")
	}
	if d.Before(before.Add(25*time.Second)) || d.After(after.Add(31*time.Second)) {
		t.Errorf("write deadline %v from the Write, want about 30 s after it (between %v and %v)", d.Sub(before), 25*time.Second, 31*time.Second)
	}
}
