package conn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"

	"golang.org/x/net/http2"
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

// H2Config holds H2 connection configuration.
type H2Config struct {
	MaxConcurrentStreams uint32
	InitialWindowSize    uint32
	MaxFrameSize         uint32
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

// H2State holds per-connection H2 state.
type H2State struct {
	initialized bool
	processor   *stream.Processor
	parser      *frame.Parser
	writer      *frame.Writer
	outBuf      *bytes.Buffer
	inBuf       *frameBuffer
	mu          sync.Mutex
	adapter     *h2ResponseAdapter
}

// SetRemoteAddr sets the remote address on the H2 stream manager so that
// all streams created on this connection inherit the peer address.
func (s *H2State) SetRemoteAddr(addr string) {
	s.processor.GetManager().RemoteAddr = addr
}

// NewH2State creates a new H2 connection state.
func NewH2State(handler stream.Handler, cfg H2Config, write func([]byte)) *H2State {
	cfg = cfg.withDefaults()

	var outBuf bytes.Buffer
	var inBuf frameBuffer
	fw := frame.NewWriter(&outBuf)

	rw := &h2ResponseAdapter{
		write:   write,
		outBuf:  &outBuf,
		writer:  fw,
		encoder: frame.NewHeaderEncoder(),
	}

	proc := stream.NewProcessor(handler, fw, rw)

	p := frame.NewParser()
	p.InitReader(&inBuf)

	proc.GetManager().SetMaxConcurrentStreams(cfg.MaxConcurrentStreams)

	return &H2State{
		processor: proc,
		parser:    p,
		writer:    fw,
		outBuf:    &outBuf,
		inBuf:     &inBuf,
		adapter:   rw,
	}
}

// ProcessH2 processes incoming H2 data.
// On first call, validates the client preface and sends server settings.
func ProcessH2(ctx context.Context, data []byte, state *H2State, _ stream.Handler,
	write func([]byte), cfg H2Config) error {

	cfg = cfg.withDefaults()

	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.initialized {
		if len(data) < len(frame.ClientPreface) {
			return fmt.Errorf("incomplete H2 client preface")
		}
		if !frame.ValidateClientPreface(data) {
			return fmt.Errorf("invalid H2 client preface")
		}
		data = data[len(frame.ClientPreface):]
		state.initialized = true

		settings := []http2.Setting{
			{ID: http2.SettingMaxConcurrentStreams, Val: cfg.MaxConcurrentStreams},
			{ID: http2.SettingInitialWindowSize, Val: cfg.InitialWindowSize},
			{ID: http2.SettingMaxFrameSize, Val: cfg.MaxFrameSize},
		}
		if err := state.writer.WriteSettings(settings...); err != nil {
			return fmt.Errorf("failed to write server settings: %w", err)
		}
		if err := state.writer.Flush(); err != nil {
			return err
		}
		flushOutBuf(state.outBuf, write)
	}

	if len(data) == 0 {
		return nil
	}

	// Write data to the input buffer, then drain all complete frames.
	// We check hasCompleteFrame before ReadNextFrame to avoid the x/net
	// framer consuming partial frame data on TCP segment boundaries
	// (which causes io.ErrUnexpectedEOF and kills the connection).
	state.inBuf.Write(data)

	for state.inBuf.hasCompleteFrame() {
		f, err := state.parser.ReadNextFrame()
		if err != nil {
			if err == io.EOF {
				break
			}
			// RFC 7540 §5.4.1: send GOAWAY before closing on connection errors.
			if ce, ok := err.(http2.ConnectionError); ok {
				_ = state.processor.SendGoAway(
					state.processor.GetManager().GetLastStreamID(),
					http2.ErrCode(ce), []byte(ce.Error()))
				flushOutBuf(state.outBuf, write)
			}
			return fmt.Errorf("frame read error: %w", err)
		}
		if err := state.processor.ProcessFrame(ctx, f); err != nil {
			flushOutBuf(state.outBuf, write)
			return err
		}
		flushOutBuf(state.outBuf, write)
	}
	return nil
}

// CloseH2 cleans up H2 state.
func CloseH2(state *H2State) {
	if state.adapter != nil && state.adapter.encoder != nil {
		state.adapter.encoder.Close()
	}
}

func flushOutBuf(buf *bytes.Buffer, write func([]byte)) {
	if buf.Len() > 0 {
		// Write directly from buffer — safe because makeWriteFn copies into its
		// own queue before returning. The buffer is reset after write returns.
		write(buf.Bytes())
		buf.Reset()
	}
}
