package conn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"
)

// frameBuffer wraps bytes.Buffer for incremental H2 frame parsing.
type frameBuffer struct {
	bytes.Buffer
}

// hasCompleteFrame peeks at the frame header to check if a complete frame
// (9-byte header + payload) is available. This prevents the x/net framer
// from consuming partial data on TCP segment boundaries, which would cause
// io.ErrUnexpectedEOF and close the connection.
func (fb *frameBuffer) hasCompleteFrame() bool {
	b := fb.Bytes()
	if len(b) < 9 {
		return false
	}
	length := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	return uint32(len(b)) >= 9+length
}

// hasMoreThanOneFrame checks if the buffer contains more than one complete frame.
// Used to suppress inline handler execution when subsequent frames need processing.
func (fb *frameBuffer) hasMoreThanOneFrame() bool {
	b := fb.Bytes()
	if len(b) < 9 {
		return false
	}
	firstLen := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	consumed := 9 + firstLen
	return uint32(len(b)) > consumed+8 // room for at least another 9-byte header
}

// H2Config holds H2 connection configuration.
type H2Config struct {
	MaxConcurrentStreams uint32
	InitialWindowSize    uint32
	MaxFrameSize         uint32
	MaxRequestBodySize   int64 // 0 = use default (100 MB)
}

// withDefaults returns a copy of cfg with zero fields set to RFC 7540 defaults.
func (cfg H2Config) withDefaults() H2Config {
	if cfg.MaxConcurrentStreams == 0 {
		cfg.MaxConcurrentStreams = 100
	}
	if cfg.InitialWindowSize == 0 {
		cfg.InitialWindowSize = 65535
	}
	if cfg.MaxFrameSize == 0 {
		cfg.MaxFrameSize = 16384
	}
	return cfg
}

// h2FrameBufPool pools pre-encoded frame byte buffers to eliminate per-response allocations.
var h2FrameBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 256); return &b },
}

func getH2FrameBuf() *[]byte { return h2FrameBufPool.Get().(*[]byte) }
func putH2FrameBuf(p *[]byte) {
	if cap(*p) > 8192 {
		return // don't pool oversized buffers
	}
	*p = (*p)[:0]
	h2FrameBufPool.Put(p)
}

// h2QueueShards is the number of shards in the write queue.
// 4 shards reduces contention by ~4x while preserving per-stream ordering.
const h2QueueShards = 4

// h2ShardedQueue is a sharded write queue for pre-encoded H2 frame bytes.
// Handler goroutines enqueue response frame data; the event loop drains it.
// Sharding by stream ID eliminates cross-stream lock contention.
type h2ShardedQueue struct {
	shards   [h2QueueShards]h2QueueShard
	pending  atomic.Bool
	wakeupFD int // eventfd for signaling event loop (-1 if unavailable)
}

type h2QueueShard struct {
	mu    sync.Mutex
	bufs  []*[]byte
	spare []*[]byte
}

// Enqueue appends pre-encoded frame bytes to the write queue.
// Called from handler goroutines. Shards by stream ID.
func (q *h2ShardedQueue) Enqueue(streamID uint32, data *[]byte) {
	shard := &q.shards[streamID%h2QueueShards]
	shard.mu.Lock()
	shard.bufs = append(shard.bufs, data)
	shard.mu.Unlock()
	// CAS coalescing: only signal the event loop if no prior enqueue already
	// set pending. If CAS fails, pending is already true and a prior enqueue
	// already wrote the eventfd — the event loop will drain all shards.
	if q.pending.CompareAndSwap(false, true) && q.wakeupFD >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(q.wakeupFD, val[:])
	}
}

// DrainTo drains all enqueued data by calling write for each buffer,
// then returns buffers to the pool.
// Called from the event loop thread. The write function must not block.
func (q *h2ShardedQueue) DrainTo(write func([]byte)) {
	for i := range q.shards {
		s := &q.shards[i]
		s.mu.Lock()
		s.spare, s.bufs = s.bufs, s.spare[:0]
		s.mu.Unlock()
		for _, buf := range s.spare {
			write(*buf)
			putH2FrameBuf(buf)
		}
		// Reclaim capacity if it grew beyond steady-state.
		if cap(s.spare) > 64 {
			s.spare = make([]*[]byte, 0, 16)
		} else {
			s.spare = s.spare[:0]
		}
	}
	q.pending.Store(false)
}

// h2StreamEncoder provides goroutine-local HPACK encoding with dynamic table
// size = 0, eliminating serialization requirements. Each goroutine gets its
// own encoder from the pool, encodes independently, and returns it.
type h2StreamEncoder struct {
	hpackBuf bytes.Buffer
	hpackEnc *hpack.Encoder
}

var h2StreamEncoderPool = sync.Pool{
	New: func() any {
		// hpackEnc is lazily initialized in encodeHeaders on the slow path
		// so encoders returned to the pool on fast-path-only workloads
		// don't carry a ~6 KB dynamic-table allocation they never used.
		return &h2StreamEncoder{}
	},
}

var (
	hpackStatus200   []byte // :status: 200
	hpackCTTextPlain []byte // content-type: text/plain
	hpackCTAppJSON   []byte // content-type: application/json
)

func init() {
	enc := &h2StreamEncoder{}
	enc.hpackEnc = hpack.NewEncoder(&enc.hpackBuf)
	enc.hpackEnc.SetMaxDynamicTableSize(0)

	enc.hpackBuf.Reset()
	_ = enc.hpackEnc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	hpackStatus200 = append([]byte(nil), enc.hpackBuf.Bytes()...)

	enc.hpackBuf.Reset()
	_ = enc.hpackEnc.WriteField(hpack.HeaderField{Name: "content-type", Value: "text/plain"})
	hpackCTTextPlain = append([]byte(nil), enc.hpackBuf.Bytes()...)

	enc.hpackBuf.Reset()
	_ = enc.hpackEnc.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/json"})
	hpackCTAppJSON = append([]byte(nil), enc.hpackBuf.Bytes()...)
}

// appendHPACKContentLength manually encodes "content-length: <value>" in HPACK
// format (literal without indexing, indexed name). This avoids the full HPACK
// encoder's static table lookup and Huffman decision overhead (~100-200ns).
// content-length is static table index 28.
func appendHPACKContentLength(buf *bytes.Buffer, value string) {
	// Literal header field without indexing (0000 prefix), indexed name.
	// Index 28 with 4-bit prefix: 28 > 14, so multi-byte: 0x0f, 28-15=13.
	buf.Write([]byte{0x0f, 0x0d})
	// Value: length-prefixed string, no Huffman (short digit strings are
	// not compressible). Bit 7 = 0 (no Huffman), bits 6-0 = length.
	n := len(value)
	if n < 127 {
		buf.WriteByte(byte(n))
	} else {
		// Multi-byte length (7-bit prefix overflow). Rare for content-length.
		buf.WriteByte(0x7f)
		rem := n - 127
		for rem >= 128 {
			buf.WriteByte(byte(rem&0x7f) | 0x80)
			rem >>= 7
		}
		buf.WriteByte(byte(rem))
	}
	buf.WriteString(value)
}

func getH2StreamEncoder() *h2StreamEncoder {
	return h2StreamEncoderPool.Get().(*h2StreamEncoder)
}

func putH2StreamEncoder(enc *h2StreamEncoder) {
	// Discard encoders with bloated buffers instead of returning to pool.
	if enc.hpackBuf.Cap() > 4096 {
		return
	}
	enc.hpackBuf.Reset()
	h2StreamEncoderPool.Put(enc)
}

// encodeHeaders HPACK-encodes response headers using dynamic table size = 0.
// Returns a slice backed by the encoder's internal buffer (valid until Reset).
// Lazily initializes e.hpackEnc the first time the slow path is taken, so the
// pre-encoded fast path (status 200 + known content-type + content-length) can
// return without ever allocating the encoder's internal dynamic table. On
// workloads like the h2c upgrade bench (where every response matches the fast
// path), this saves ~8% of server-side allocations per connection.
func (e *h2StreamEncoder) encodeHeaders(headers [][2]string) ([]byte, error) {
	if e.hpackEnc == nil {
		e.hpackEnc = hpack.NewEncoder(&e.hpackBuf)
		e.hpackEnc.SetMaxDynamicTableSize(0)
	}
	e.hpackBuf.Reset()
	for _, h := range headers {
		if err := e.hpackEnc.WriteField(hpack.HeaderField{Name: h[0], Value: h[1]}); err != nil {
			return nil, err
		}
	}
	return e.hpackBuf.Bytes(), nil
}

// H2 frame building helpers — produce raw frame bytes without shared state.
// These replace the shared frame.Writer for the response hot path.

const (
	h2FrameData         byte = 0x00
	h2FrameHeaders      byte = 0x01
	h2FrameRSTStream    byte = 0x03
	h2FrameContinuation byte = 0x09

	h2FlagEndStream  byte = 0x01
	h2FlagEndHeaders byte = 0x04
)

// writeH2FrameHeader writes a 9-byte H2 frame header directly to a Buffer.
// For HEADERS frames, sets END_HEADERS. For DATA/HEADERS with endStream, sets END_STREAM.
// Used by inline response adapters to avoid intermediate slice allocations.
func writeH2FrameHeader(buf *bytes.Buffer, frameType byte, endStream bool, streamID uint32, payloadLen int) {
	var flags byte
	if endStream {
		flags |= h2FlagEndStream
	}
	if frameType == h2FrameHeaders {
		flags |= h2FlagEndHeaders
	}
	var hdr [9]byte
	hdr[0] = byte(payloadLen >> 16)
	hdr[1] = byte(payloadLen >> 8)
	hdr[2] = byte(payloadLen)
	hdr[3] = frameType
	hdr[4] = flags
	hdr[5] = byte(streamID >> 24)
	hdr[6] = byte(streamID >> 16)
	hdr[7] = byte(streamID >> 8)
	hdr[8] = byte(streamID)
	buf.Write(hdr[:])
}

// appendH2Frame appends a raw H2 frame (9-byte header + payload) to buf.
func appendH2Frame(buf []byte, frameType byte, flags byte, streamID uint32, payload []byte) []byte {
	length := len(payload)
	buf = append(buf,
		byte(length>>16), byte(length>>8), byte(length),
		frameType,
		flags,
		byte(streamID>>24), byte(streamID>>16), byte(streamID>>8), byte(streamID),
	)
	return append(buf, payload...)
}

// appendH2Headers appends HEADERS (+ CONTINUATION if needed) frames to buf.
func appendH2Headers(buf []byte, streamID uint32, endStream bool, headerBlock []byte, maxFrameSize uint32) []byte {
	if maxFrameSize == 0 {
		maxFrameSize = 16384
	}

	remaining := headerBlock
	first := true
	for len(remaining) > 0 {
		chunkLen := int(maxFrameSize)
		if len(remaining) < chunkLen {
			chunkLen = len(remaining)
		}
		frag := remaining[:chunkLen]
		remaining = remaining[chunkLen:]

		if first {
			var flags byte
			if endStream {
				flags |= h2FlagEndStream
			}
			if len(remaining) == 0 {
				flags |= h2FlagEndHeaders
			}
			buf = appendH2Frame(buf, h2FrameHeaders, flags, streamID, frag)
			first = false
		} else {
			var flags byte
			if len(remaining) == 0 {
				flags |= h2FlagEndHeaders
			}
			buf = appendH2Frame(buf, h2FrameContinuation, flags, streamID, frag)
		}
	}
	return buf
}

// appendH2Data appends a DATA frame to buf.
func appendH2Data(buf []byte, streamID uint32, endStream bool, data []byte) []byte {
	var flags byte
	if endStream {
		flags |= h2FlagEndStream
	}
	return appendH2Frame(buf, h2FrameData, flags, streamID, data)
}

// H2State holds per-connection H2 state.
type H2State struct {
	initialized       bool
	serverPrefaceSent bool // true if server SETTINGS already written (h2c upgrade path)
	prefaceBuf        []byte
	processor         *stream.Processor
	parser            *frame.Parser
	writer            *frame.Writer
	outBuf            *bytes.Buffer
	inBuf             *frameBuffer
	mu                sync.Mutex
	adapter           *h2ResponseAdapter
	inlineAdapter     *h2InlineResponseAdapter
	writeQueue        *h2ShardedQueue
	cfg               H2Config // cached with defaults applied
}

// SetRemoteAddr sets the remote address on the H2 stream manager so that
// all streams created on this connection inherit the peer address.
func (s *H2State) SetRemoteAddr(addr string) {
	s.processor.GetManager().RemoteAddr = addr
}

// WriteQueuePending returns true if the write queue has pending data.
func (s *H2State) WriteQueuePending() bool {
	return s.writeQueue.pending.Load()
}

// DrainWriteQueue drains async handler responses to the write function.
// Called from the event loop thread (outside H2State.mu).
func (s *H2State) DrainWriteQueue(write func([]byte)) {
	if s.writeQueue.pending.Load() {
		s.writeQueue.DrainTo(write)
	}
}

// NewH2State creates a new H2 connection state. wakeupFD is an eventfd used
// to signal the event loop when handler goroutines enqueue responses (-1 to
// disable, falling back to polling).
func NewH2State(handler stream.Handler, cfg H2Config, write func([]byte), wakeupFD int) *H2State {
	cfg = cfg.withDefaults()

	var outBuf bytes.Buffer
	var inBuf frameBuffer
	fw := frame.NewWriter(&outBuf)

	wq := &h2ShardedQueue{wakeupFD: wakeupFD}

	rw := &h2ResponseAdapter{
		write:        write,
		outBuf:       &outBuf,
		writer:       fw,
		writeQueue:   wq,
		maxFrameSize: cfg.MaxFrameSize,
	}

	inlineRW := &h2InlineResponseAdapter{
		outBuf:       &outBuf,
		maxFrameSize: cfg.MaxFrameSize,
	}
	// enc.hpackEnc is lazily initialized in encodeHeaders on the slow path.
	// The fast path (status 200 + known content-type + content-length) uses
	// pre-encoded HPACK bytes and never needs the dynamic encoder — see
	// h2InlineResponseAdapter.WriteResponse.

	proc := stream.NewProcessor(handler, fw, rw)
	proc.InlineWriter = inlineRW
	proc.MaxRequestBodySize = cfg.MaxRequestBodySize

	p := frame.NewParser()
	p.InitReader(&inBuf)

	proc.GetManager().SetMaxConcurrentStreams(cfg.MaxConcurrentStreams)

	return &H2State{
		processor:     proc,
		parser:        p,
		writer:        fw,
		outBuf:        &outBuf,
		inBuf:         &inBuf,
		adapter:       rw,
		inlineAdapter: inlineRW,
		writeQueue:    wq,
		cfg:           cfg,
	}
}

// NewH2StateFromUpgrade constructs an H2State that has already absorbed
// the RFC 7540 §3.2 h2c upgrade: the server preface (SETTINGS + ACK) is
// written synchronously, the client's HTTP2-Settings are applied, and the
// original H1 request is injected as stream 1. The engine then feeds
// info.Remaining — which may contain the expected H2 client preface and
// initial SETTINGS frame — through ProcessH2 on the same iteration.
//
// Unlike NewH2State, this function writes to the connection directly
// (before returning) because the 101 response has already been sent and
// the client is entitled to see the server SETTINGS immediately.
func NewH2StateFromUpgrade(handler stream.Handler, cfg H2Config, write func([]byte), wakeupFD int, info *UpgradeInfo) (*H2State, error) {
	state := NewH2State(handler, cfg, write, wakeupFD)

	// Apply client's settings from the HTTP2-Settings header. Settings
	// payload is a sequence of {u16 id, u32 val} pairs (already validated
	// by DecodeHTTP2Settings to be a multiple of 6 bytes).
	mgr := state.processor.GetManager()
	for i := 0; i+6 <= len(info.Settings); i += 6 {
		id := uint16(info.Settings[i])<<8 | uint16(info.Settings[i+1])
		val := uint32(info.Settings[i+2])<<24 | uint32(info.Settings[i+3])<<16 |
			uint32(info.Settings[i+4])<<8 | uint32(info.Settings[i+5])
		mgr.ApplySetting(id, val)
	}

	// Write server preface: a SETTINGS frame with our parameters, and a
	// SETTINGS ACK for the client's settings we just applied. Order is
	// not strictly specified (RFC 7540 §3.5: "The server connection
	// preface consists of a potentially empty SETTINGS frame..."), but
	// sending our SETTINGS first matches common server behavior.
	settings := []http2.Setting{
		{ID: http2.SettingMaxConcurrentStreams, Val: state.cfg.MaxConcurrentStreams},
		{ID: http2.SettingInitialWindowSize, Val: state.cfg.InitialWindowSize},
		{ID: http2.SettingMaxFrameSize, Val: state.cfg.MaxFrameSize},
	}
	if err := state.writer.WriteSettings(settings...); err != nil {
		return nil, fmt.Errorf("h2c upgrade: write server settings: %w", err)
	}
	if err := state.writer.WriteSettingsAck(); err != nil {
		return nil, fmt.Errorf("h2c upgrade: write settings ack: %w", err)
	}
	if err := state.writer.Flush(); err != nil {
		return nil, fmt.Errorf("h2c upgrade: flush settings: %w", err)
	}
	flushOutBuf(state.outBuf, write)
	state.serverPrefaceSent = true

	// Extract authority (Host) from headers (lowercase).
	authority := ""
	for _, h := range info.Headers {
		if h[0] == "host" {
			authority = h[1]
			break
		}
	}

	// Build stream 1 header list: H2 pseudo-headers first, then regular
	// headers (H2 requires pseudos precede all others).
	h2Headers := make([][2]string, 0, len(info.Headers)+4)
	h2Headers = append(h2Headers,
		[2]string{":method", info.Method},
		[2]string{":path", info.URI},
		[2]string{":scheme", "http"},
		[2]string{":authority", authority},
	)
	for _, h := range info.Headers {
		// Skip Host — represented as :authority above. Other hop-by-hop
		// headers were stripped at copyH2CHeaders time.
		if h[0] == "host" {
			continue
		}
		h2Headers = append(h2Headers, h)
	}

	// The H1 request was fully buffered before upgrade; whatever body we
	// have is the complete body. Always end-stream on the injected stream.
	if err := state.processor.InjectStreamHeaders(1, true, h2Headers, info.Body); err != nil {
		return nil, fmt.Errorf("h2c upgrade: inject stream 1: %w", err)
	}
	// Drain any inline response that the handler produced (common case for
	// simple GET handlers that write immediately).
	flushOutBuf(state.outBuf, write)
	if state.writeQueue.pending.Load() {
		state.writeQueue.DrainTo(write)
	}
	return state, nil
}

// ProcessH2 processes incoming H2 data.
// On first call, validates the client preface and sends server settings.
// PING, SETTINGS ACK) and falls back to x/net framer for complex types.
func ProcessH2(ctx context.Context, data []byte, state *H2State, _ stream.Handler,
	write func([]byte), _ H2Config) error {

	state.mu.Lock()

	if !state.initialized {
		// h2c upgrade path writes the server preface (SETTINGS) synchronously
		// in NewH2StateFromUpgrade, so skip the write if already done.
		if !state.serverPrefaceSent {
			settings := []http2.Setting{
				{ID: http2.SettingMaxConcurrentStreams, Val: state.cfg.MaxConcurrentStreams},
				{ID: http2.SettingInitialWindowSize, Val: state.cfg.InitialWindowSize},
				{ID: http2.SettingMaxFrameSize, Val: state.cfg.MaxFrameSize},
			}
			if err := state.writer.WriteSettings(settings...); err != nil {
				state.mu.Unlock()
				return fmt.Errorf("failed to write server settings: %w", err)
			}
			if err := state.writer.Flush(); err != nil {
				state.mu.Unlock()
				return err
			}
			flushOutBuf(state.outBuf, write)
			state.serverPrefaceSent = true
		}

		// Tolerate a partial client preface. The h2c upgrade seam often
		// lands the trailing preface bytes across two recv buffers — when
		// that happens we stash what we have and wait for more. Compare
		// strictly: any byte that contradicts the preface is an immediate
		// connection error.
		combined := data
		if len(state.prefaceBuf) > 0 {
			combined = append(state.prefaceBuf, data...)
		}
		pfx := combined
		if len(pfx) > len(frame.ClientPreface) {
			pfx = pfx[:len(frame.ClientPreface)]
		}
		if string(pfx) != frame.ClientPreface[:len(pfx)] {
			state.mu.Unlock()
			return fmt.Errorf("invalid H2 client preface")
		}
		if len(combined) < len(frame.ClientPreface) {
			// Buffer-and-wait: strict prefix match valid; need more bytes.
			state.prefaceBuf = append(state.prefaceBuf[:0], combined...)
			state.mu.Unlock()
			state.DrainWriteQueue(write)
			return nil
		}
		data = combined[len(frame.ClientPreface):]
		state.prefaceBuf = nil
		state.initialized = true
	}

	if len(data) == 0 {
		state.mu.Unlock()
		state.DrainWriteQueue(write)
		return nil
	}

	state.inBuf.Write(data)

	maxFrameSize := state.processor.GetManager().GetMaxFrameSize()
	for state.inBuf.hasCompleteFrame() {
		// Tell the processor if more frames follow in this recv buffer.
		// When true, canRunInline returns false to avoid sending the
		// response before subsequent frames (WINDOW_UPDATE, PRIORITY)
		// are processed — required for h2spec compliance.
		state.processor.SetHasMoreFrames(state.inBuf.hasMoreThanOneFrame())

		// Fast path: peek at frame header to detect simple HEADERS frames
		// (END_HEADERS set, no PADDED, no PRIORITY, not in CONTINUATION).
		// Bypasses x/net framer's ReadFrame which allocates *HeadersFrame.
		var processErr error
		raw := state.inBuf.Bytes()
		frameType := raw[3]
		frameFlags := raw[4]
		frameLen := uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])
		const (
			flagEndStream  = 0x01
			flagEndHeaders = 0x04
			flagPadded     = 0x08
			flagPriority   = 0x20
			typeHeaders    = 0x01
		)
		if frameType == typeHeaders &&
			frameFlags&flagEndHeaders != 0 &&
			frameFlags&flagPadded == 0 &&
			frameFlags&flagPriority == 0 &&
			frameLen <= maxFrameSize &&
			!state.processor.IsExpectingContinuation() {
			// Zero-alloc HEADERS fast path.
			rf, consumed, _ := frame.ReadRawFrame(raw)
			state.inBuf.Next(consumed) // advance past frame
			processErr = state.processor.ProcessRawHeaders(
				rf.StreamID, rf.HasEndStream(), rf.Payload)
		} else {
			// Standard path: delegate to x/net framer for complex frames.
			f, err := state.parser.ReadNextFrame()
			if err != nil {
				if err == io.EOF {
					break
				}
				if ce, ok := err.(http2.ConnectionError); ok {
					_ = state.processor.SendGoAway(
						state.processor.GetManager().GetLastStreamID(),
						http2.ErrCode(ce), []byte(ce.Error()))
					flushOutBuf(state.outBuf, write)
				}
				state.mu.Unlock()
				state.DrainWriteQueue(write)
				return fmt.Errorf("frame read error: %w", err)
			}
			processErr = state.processor.ProcessFrame(ctx, f)
		}
		if processErr != nil {
			flushOutBuf(state.outBuf, write)
			state.mu.Unlock()
			state.DrainWriteQueue(write)
			return processErr
		}
		flushOutBuf(state.outBuf, write)
		// Drain inline handler responses immediately so they're sent in the
		// same event loop iteration. Without this, responses queue up and
		// never flush until the frame loop exits (starving h2load clients).
		if state.writeQueue.pending.Load() {
			state.writeQueue.DrainTo(write)
		}
	}

	// Clean up inline-completed streams that were deferred during the
	// frame loop (pending outbound data or more frames to process).
	state.processor.FlushInlineCleanup()

	state.mu.Unlock()

	// Drain async handler responses that completed during frame parsing.
	state.DrainWriteQueue(write)

	return nil
}

// CloseH2 cleans up H2 state. Releases all streams still held by the
// manager to prevent memory leaks on connection close.
func CloseH2(state *H2State) {
	state.processor.GetManager().Close()
}

func flushOutBuf(buf *bytes.Buffer, write func([]byte)) {
	if buf.Len() > 0 {
		// Write directly from buffer — safe because makeWriteFn copies into its
		// own queue before returning. The buffer is reset after write returns.
		write(buf.Bytes())
		buf.Reset()
	}
}
