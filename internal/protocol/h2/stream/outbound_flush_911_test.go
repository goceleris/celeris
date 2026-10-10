package stream

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/net/http2"
)

// celeris#911: a peer that grants a stream's window a byte at a time while the
// stream holds a large buffered body must cost the server work in proportion
// to the frame, not to the body. flushStreamOutbound and the
// SETTINGS_INITIAL_WINDOW_SIZE re-flush used to copy the unsent remainder out
// of the stream's OutboundBuffer and back in on every partial send: about one
// body's worth of allocation (4,195,040 bytes for a 4 MiB body) per 13-byte
// WINDOW_UPDATE.

const dribbleBody911 = 4 << 20

// dribbleSetup911 returns a Processor with a stream (ID 1) that holds
// dribbleBody911 bytes of a recognisable pattern, a zero stream window and an
// open connection window.
func dribbleSetup911(t *testing.T) (*Processor, *Stream, *testFrameWriter, []byte) {
	t.Helper()
	return dribbleSetupSize911(t, dribbleBody911)
}

func dribbleSetupSize911(t testing.TB, size int) (*Processor, *Stream, *testFrameWriter, []byte) {
	t.Helper()
	fw := newTestFrameWriter()
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), fw, newTestResponseWriter())
	m := p.GetManager()
	m.UpdateConnectionWindow(1 << 30)
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(0)
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251)
	}
	s.BufferOutbound(body, true)
	return p, s, fw, body
}

// dribble911 runs 200 one-byte grants through grant and checks, for each
// grant path:
//   - the bytes allocated per grant stay far below the body (the defect
//     allocated one body per grant, 4,195,040 bytes);
//   - the buffer is not compacted: its first unsent byte sits exactly one
//     byte further on after each grant, so no grant moved the remainder (an
//     in-place copy of the remainder would pass an allocation bound and still
//     move the whole body every time);
//   - each grant sent exactly the one byte it granted, the right one.
func dribble911(t *testing.T, path string, grant func(t *testing.T, p *Processor, s *Stream, i int)) {
	const updates = 200
	p, s, fw, body := dribbleSetup911(t)
	first := unsafe.Pointer(&s.OutboundBuffer.Bytes()[0])
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	for i := 0; i < updates; i++ {
		grant(t, p, s, i)
	}
	runtime.ReadMemStats(&b)
	per := float64(b.TotalAlloc-a.TotalAlloc) / updates

	left := s.OutboundBuffer.Len()
	t.Logf("%s: %d one-byte grants with a %d-byte body buffered: %d bytes sent, %d left, %.0f bytes allocated per grant",
		path, updates, dribbleBody911, dribbleBody911-left, left, per)
	if sent := dribbleBody911 - left; sent != updates {
		t.Fatalf("%s: %d bytes sent, want %d (one per grant)", path, sent, updates)
	}
	if per > 4096 {
		t.Errorf("%s: %.0f bytes allocated per one-byte grant with a %d-byte body buffered, want well under 4096: the server copies the rest of the body for every frame", path, per, dribbleBody911)
	}
	if got := unsafe.Pointer(&s.OutboundBuffer.Bytes()[0]); got != unsafe.Add(first, updates) {
		t.Errorf("%s: the first unsent byte moved %d bytes in the buffer after %d one-byte grants, want %d: the remainder was moved, not dropped in place",
			path, int(uintptr(got))-int(uintptr(first)), updates, updates)
	}
	if m := p.GetManager().OutboundHeld(); m != int64(left) {
		t.Errorf("%s: the outbound budget holds %d bytes, want %d (what the stream still buffers)", path, m, left)
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	var sent []byte
	for _, d := range fw.dataSent {
		if d.streamID != 1 || len(d.data) != 1 || d.endStream {
			t.Fatalf("%s: unexpected DATA record %+v", path, d)
		}
		sent = append(sent, d.data...)
	}
	if !bytes.Equal(sent, body[:updates]) {
		t.Errorf("%s: the bytes sent are not the first %d bytes of the body, in order", path, updates)
	}
}

// TestStreamWindowUpdateDribbleDoesNotCopyTheBody911 is the stream
// WINDOW_UPDATE path (flushStreamOutbound).
func TestStreamWindowUpdateDribbleDoesNotCopyTheBody911(t *testing.T) {
	dribble911(t, "stream WINDOW_UPDATE", func(t *testing.T, p *Processor, _ *Stream, _ int) {
		if err := p.ProcessFrame(context.Background(), makeWindowUpdateFrame(t, 1, 1)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestConnWindowUpdateDribbleDoesNotCopyTheBody911 is the connection
// WINDOW_UPDATE path (flushConnWindowStalledStreams): the stream's window is
// open and the connection window is what the peer dribbles.
func TestConnWindowUpdateDribbleDoesNotCopyTheBody911(t *testing.T) {
	dribble911(t, "connection WINDOW_UPDATE", func(t *testing.T, p *Processor, s *Stream, i int) {
		if i == 0 {
			// The stream window has room for the whole run; the connection
			// window is empty, and each grant gives it one byte.
			s.SetWindowSize(1 << 30)
			atomic.StoreInt32(&p.GetManager().connectionWindow, 0)
		}
		if err := p.ProcessFrame(context.Background(), makeWindowUpdateFrame(t, 0, 1)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestSettingsDribbleDoesNotCopyTheBody911 is the SETTINGS_INITIAL_WINDOW_SIZE
// re-flush in handleSettings: each SETTINGS frame raises the initial window by
// one, which credits every stream one byte.
func TestSettingsDribbleDoesNotCopyTheBody911(t *testing.T) {
	dribble911(t, "SETTINGS_INITIAL_WINDOW_SIZE", func(t *testing.T, p *Processor, _ *Stream, i int) {
		f := makeSettingsFrame(t, http2.Setting{ID: http2.SettingInitialWindowSize, Val: uint32(65535 + 1 + i)})
		if err := p.ProcessFrame(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	})
}

// BenchmarkOneByteGrant911 is the cost of one one-byte stream WINDOW_UPDATE
// while the stream holds a body of the given size: it was in proportion to the
// body (the remainder copied twice), and is now in proportion to the frame.
func BenchmarkOneByteGrant911(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 4 << 20} {
		b.Run(byteSize911(size), func(b *testing.B) {
			p, s, _, body := dribbleSetupSize911(b, size)
			wu := makeWindowUpdateFrame(&testing.T{}, 1, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if s.OutboundBuffer.Len() < 2 { // never the last byte: that would end and release the stream
					b.StopTimer()
					s.BufferOutbound(body, true)
					b.StartTimer()
				}
				if err := p.ProcessFrame(context.Background(), wu); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func byteSize911(n int) string {
	if n >= 1<<20 {
		return strconv.Itoa(n>>20) + "MiB"
	}
	return strconv.Itoa(n>>10) + "KiB"
}
