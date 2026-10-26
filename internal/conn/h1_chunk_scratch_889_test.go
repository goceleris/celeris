package conn

import (
	"bytes"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The chunk scratch is bounded: a chunk over maxChunkScratch is framed in a
// buffer of its own, and a connection whose handler streamed a large chunk and
// returned does not keep a scratch over chunkScratchKeep for its next requests
// (celeris#889 item 54).
func TestH1ChunkScratchIsBounded889(t *testing.T) {
	var out bytes.Buffer
	a := &h1ResponseAdapter{write: func(p []byte) { out.Write(p) }, keepAlive: true}
	_ = a.WriteHeader(nil, 200, nil)
	big := bytes.Repeat([]byte("y"), maxChunkScratch+1)
	if err := a.Write(nil, big); err != nil {
		t.Fatal(err)
	}
	if cap(a.chunkBuf) > maxChunkScratch {
		t.Errorf("a chunk over the bound grew the scratch to %d", cap(a.chunkBuf))
	}
	if !bytes.HasSuffix(out.Bytes(), append(append([]byte{}, big...), '\r', '\n')) {
		t.Error("the large chunk is not framed whole")
	}

	h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		_ = st.WriteHeader(s, 200, nil)
		return st.Write(s, bytes.Repeat([]byte("y"), 32<<10))
	})
	state := NewH1State()
	if _, err := feed889(t, state, h, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if c := cap(state.rw.chunkBuf); c > chunkScratchKeep {
		t.Errorf("the connection keeps a %d-byte scratch after the handler returned, want at most %d", c, chunkScratchKeep)
	}
}

// A detached stream's writer lives as long as its connection (SSE, long-poll):
// it holds at most chunkScratchKeep of scratch, whatever the largest event it
// sent, and what it held before the detach is dropped (a broadcast of one
// large event must not pin a buffer on every connection).
func TestH1DetachedWriterKeepsASmallScratch889(t *testing.T) {
	state := NewH1State()
	var out bytes.Buffer
	state.rw.write = func(p []byte) { out.Write(p) }
	state.rw.keepAlive = true
	a := &state.rw
	_ = a.WriteHeader(nil, 200, nil)
	_ = a.Write(nil, bytes.Repeat([]byte("e"), 40<<10)) // before the detach: kept for the next chunk
	if cap(a.chunkBuf) <= chunkScratchKeep {
		t.Fatalf("setup: the scratch is %d bytes", cap(a.chunkBuf))
	}
	state.UpdateWriteFn(func(p []byte) { out.Write(p) }) // what OnDetach does
	if cap(a.chunkBuf) > chunkScratchKeep {
		t.Errorf("the scratch is %d bytes after the detach, want at most %d", cap(a.chunkBuf), chunkScratchKeep)
	}
	out.Reset()
	big := bytes.Repeat([]byte("e"), 60<<10)
	_ = a.Write(nil, big)
	if cap(a.chunkBuf) > chunkScratchKeep {
		t.Errorf("a 60 KiB event on a detached writer left a %d-byte scratch, want at most %d", cap(a.chunkBuf), chunkScratchKeep)
	}
	if !bytes.Equal(out.Bytes(), append(append([]byte("f000\r\n"), big...), '\r', '\n')) {
		t.Errorf("the 60 KiB event is not framed whole: %.30q", out.Bytes())
	}
	// small events still cost nothing
	small := bytes.Repeat([]byte("s"), 100)
	if allocs := testing.AllocsPerRun(100, func() { out.Reset(); _ = a.Write(nil, small) }); allocs != 0 {
		t.Errorf("a small event on a detached writer: %v allocs/op, want 0", allocs)
	}
}

// A detach that was rolled back (the engine's OnDetach panicked and put the
// write hook back) leaves a connection that goes on serving ordinary requests:
// the next request does not inherit the detached writer's small scratch limit.
func TestH1ScratchLimitIsPerRequest889(t *testing.T) {
	state := NewH1State()
	state.UpdateWriteFn(func([]byte) {}) // OnDetach installed its hook ...
	if !state.rw.scratchDetached {
		t.Fatal("setup: UpdateWriteFn did not mark the writer detached")
	}
	h := streamHandler889(func(st stream.Streamer, s *stream.Stream) error {
		_ = st.WriteHeader(s, 200, nil)
		return st.Write(s, bytes.Repeat([]byte("z"), 30<<10))
	})
	_, _ = feed889(t, state, h, "GET / HTTP/1.1\r\nHost: x\r\n\r\n") // ... and a later request on the same connection
	if state.rw.scratchDetached {
		t.Error("a request on a connection that is not detached keeps the detached writer's scratch limit")
	}
}
