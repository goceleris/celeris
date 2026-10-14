//go:build linux

package celeris_test

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// h2StreamEnds837 sends one GET on a fresh h2c connection (prior knowledge)
// and waits up to d for its stream to end. After its HEADERS the client sends
// nothing but the SETTINGS ack: a frame the server leaves in its write queue is
// then never flushed by a later inbound frame. ended reports END_STREAM; data
// counts the stream's DATA payload bytes.
func h2StreamEnds837(addr, path string, d time.Duration) (ended bool, data int, err error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(d))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		return false, 0, err
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		return false, 0, err
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", path}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		return false, 0, err
	}
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return false, data, err
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := fr.WriteSettingsAck(); err != nil {
					return false, data, err
				}
			}
		case *http2.HeadersFrame:
			if f.StreamID == 1 && f.StreamEnded() {
				return true, data, nil
			}
		case *http2.DataFrame:
			if f.StreamID == 1 {
				data += len(f.Data())
				if f.StreamEnded() {
					return true, data, nil
				}
			}
		case *http2.RSTStreamFrame:
			if f.StreamID == 1 {
				return false, data, fmt.Errorf("stream reset: %v", f.ErrCode)
			}
		case *http2.GoAwayFrame:
			return false, data, fmt.Errorf("GOAWAY: %v", f.ErrCode)
		}
	}
}

// TestH2AsyncStreamEndsForQuietClient837 is celeris#837 end to end. An async
// route streams its response (StreamWriter: WriteHeader, Write, Close), so its
// frames are enqueued one by one on the HTTP/2 write queue from a pool
// goroutine while the event loop drains it. An enqueue that landed while
// DrainTo was past its shard was neither drained nor signalled, and the
// stream's last frames (the END_STREAM DATA) then stayed queued until another
// stream on the connection enqueued; a client waiting on its only stream waited
// forever. The race is narrow (about 5 streams in 1,000 on main fe9264f), so
// each engine serves streamCount837 fresh connections, every one of which must
// end within its deadline. std is the control row: net/http's HTTP/2 server has
// no such queue.
func TestH2AsyncStreamEndsForQuietClient837(t *testing.T) {
	const (
		streamCount837 = 1500
		parallel837    = 8
		deadline837    = 2 * time.Second
	)
	n := streamCount837
	if testing.Short() {
		n = 300
	}
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
				s.GET("/stream837", func(c *celeris.Context) error {
					sw := c.StreamWriter()
					if sw == nil {
						return errors.New("no StreamWriter")
					}
					if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
						return err
					}
					if _, err := sw.Write([]byte("x")); err != nil {
						return err
					}
					return sw.Close()
				}).Async()
			})
			var (
				mu                  sync.Mutex
				ended, stuck, other int
				firstOther          error
				wg                  sync.WaitGroup
			)
			jobs := make(chan struct{})
			for w := 0; w < parallel837; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range jobs {
						ok, data, err := h2StreamEnds837(addr, "/stream837", deadline837)
						var ne net.Error
						mu.Lock()
						switch {
						case ok && data == 1:
							ended++
						case !ok && errors.As(err, &ne) && ne.Timeout():
							stuck++
						default:
							other++
							if firstOther == nil {
								firstOther = fmt.Errorf("ended=%v data=%d err=%v", ok, data, err)
							}
						}
						mu.Unlock()
					}
				}()
			}
			for i := 0; i < n; i++ {
				jobs <- struct{}{}
			}
			close(jobs)
			wg.Wait()
			t.Logf("%s: async StreamWriter route, raw h2c GET, quiet client: %d of %d streams ended, %d never ended within %v, %d other",
				e.name, ended, n, stuck, deadline837, other)
			if stuck > 0 {
				t.Errorf("%s: %d of %d streams never ended within %v: their last frames stayed in the write queue (celeris#837)",
					e.name, stuck, n, deadline837)
			}
			if other > 0 {
				t.Errorf("%s: %d of %d streams failed otherwise, first: %v", e.name, other, n, firstOther)
			}
			if ended+stuck+other != n {
				t.Errorf("%s: tallied %d outcomes for %d streams", e.name, ended+stuck+other, n)
			}
		})
	}
}
