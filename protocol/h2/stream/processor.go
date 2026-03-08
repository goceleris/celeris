package stream

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// ContinuationState tracks the state of CONTINUATION frames.
type ContinuationState struct {
	streamID      uint32
	headerBlock   []byte
	endStream     bool
	expectingMore bool
	isTrailers    bool
}

var headerBlockPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

var headersSlicePoolIn = sync.Pool{New: func() any { s := make([][2]string, 0, 16); return &s }}

// Processor processes incoming HTTP/2 frames and manages streams.
type Processor struct {
	manager             *Manager
	handler             Handler
	writer              FrameWriter
	currentConn         ResponseWriter
	connWriter          ResponseWriter
	hpackDecoder        *hpack.Decoder
	continuationState   *ContinuationState
	continuationStateMu sync.Mutex
}

// NewProcessor creates a new stream processor.
func NewProcessor(handler Handler, writer FrameWriter, conn ResponseWriter) *Processor {
	return &Processor{
		manager:      NewManager(),
		handler:      handler,
		writer:       writer,
		connWriter:   conn,
		hpackDecoder: hpack.NewDecoder(4096, nil),
	}
}

// GetManager returns the stream manager.
func (p *Processor) GetManager() *Manager {
	return p.manager
}

// GetCurrentConn returns the current connection.
func (p *Processor) GetCurrentConn() ResponseWriter {
	if p.currentConn != nil {
		return p.currentConn
	}
	return p.connWriter
}

// GetConnection returns the permanent connection writer.
func (p *Processor) GetConnection() ResponseWriter {
	return p.connWriter
}

// IsExpectingContinuation reports whether the processor is in the middle of
// receiving a header block.
func (p *Processor) IsExpectingContinuation() bool {
	p.continuationStateMu.Lock()
	defer p.continuationStateMu.Unlock()
	return p.continuationState != nil && p.continuationState.expectingMore
}

// GetExpectedContinuationStreamID returns the stream ID we're expecting CONTINUATION frames on.
func (p *Processor) GetExpectedContinuationStreamID() (uint32, bool) {
	p.continuationStateMu.Lock()
	defer p.continuationStateMu.Unlock()
	if p.continuationState != nil && p.continuationState.expectingMore {
		return p.continuationState.streamID, true
	}
	return 0, false
}

// ProcessFrame processes an incoming HTTP/2 frame.
//
//nolint:gocyclo // complex frame dispatch logic
func (p *Processor) ProcessFrame(ctx context.Context, frame http2.Frame) error {
	p.continuationStateMu.Lock()
	inContinuation := p.continuationState != nil && p.continuationState.expectingMore
	expectingStreamID := uint32(0)
	if inContinuation {
		expectingStreamID = p.continuationState.streamID
	}
	p.continuationStateMu.Unlock()

	if inContinuation {
		header := frame.Header()
		if header.StreamID == expectingStreamID {
			if _, isContinuation := frame.(*http2.ContinuationFrame); !isContinuation {
				return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("non-CONTINUATION on stream expecting CONTINUATION"),
					fmt.Errorf("received non-CONTINUATION frame on stream %d while expecting CONTINUATION", header.StreamID))
			}
		} else {
			// RFC 7540 §6.10: any frame on a different stream during a header
			// block is a connection error of type PROTOCOL_ERROR.
			return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
				[]byte("frame on another stream during header block"),
				fmt.Errorf("frame type %T on stream %d while header block is open on %d", frame, header.StreamID, expectingStreamID))
		}
	}

	header := frame.Header()
	p.manager.mu.RLock()
	maxFrameSize := p.manager.maxFrameSize
	p.manager.mu.RUnlock()

	switch frame.(type) {
	case *http2.DataFrame:
		if header.Length > maxFrameSize {
			if header.StreamID == 0 {
				return p.goAwayErr(0, http2.ErrCodeFrameSize, []byte("DATA frame too large"),
					fmt.Errorf("DATA frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, maxFrameSize))
			}
			_ = p.writer.WriteRSTStream(header.StreamID, http2.ErrCodeFrameSize)
			p.flush()
			return fmt.Errorf("DATA frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, maxFrameSize)
		}
	case *http2.HeadersFrame:
		if header.Length > maxFrameSize {
			return p.goAwayErr(0, http2.ErrCodeFrameSize, []byte("HEADERS frame too large"),
				fmt.Errorf("HEADERS frame exceeds MAX_FRAME_SIZE: %d > %d", header.Length, maxFrameSize))
		}
	}

	switch f := frame.(type) {
	case *http2.SettingsFrame:
		return p.handleSettings(f)
	case *http2.HeadersFrame:
		return p.handleHeaders(ctx, f)
	case *http2.DataFrame:
		return p.handleData(ctx, f)
	case *http2.WindowUpdateFrame:
		return p.handleWindowUpdate(f)
	case *http2.RSTStreamFrame:
		return p.handleRSTStream(f)
	case *http2.PriorityFrame:
		return p.handlePriority(f)
	case *http2.GoAwayFrame:
		return p.handleGoAway(f)
	case *http2.PingFrame:
		return p.handlePing(f)
	case *http2.ContinuationFrame:
		return p.handleContinuation(ctx, f)
	case *http2.PushPromiseFrame:
		return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("client sent PUSH_PROMISE"),
			fmt.Errorf("client sent PUSH_PROMISE frame"))
	default:
		return nil
	}
}

// ProcessFrameWithConn processes a frame with a connection context.
func (p *Processor) ProcessFrameWithConn(ctx context.Context, frame http2.Frame, conn ResponseWriter) error {
	p.currentConn = conn
	defer func() { p.currentConn = nil }()

	return p.ProcessFrame(ctx, frame)
}

// handleSettings processes SETTINGS frames.
//
//nolint:gocyclo // complex settings negotiation logic
func (p *Processor) handleSettings(f *http2.SettingsFrame) error {
	if f.IsAck() {
		return nil
	}

	type bufferedFlush struct {
		streamID  uint32
		data      []byte
		endStream bool
	}

	var validationErr error
	var pendingFlushes []bufferedFlush

	_ = f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingHeaderTableSize:
			// No validation needed
		case http2.SettingEnablePush:
			if s.Val != 0 && s.Val != 1 {
				validationErr = fmt.Errorf("SETTINGS_ENABLE_PUSH must be 0 or 1, got %d", s.Val)
				return validationErr
			}
			p.manager.mu.Lock()
			p.manager.pushEnabled = s.Val == 1
			p.manager.mu.Unlock()
		case http2.SettingMaxConcurrentStreams:
			// Client's MAX_CONCURRENT_STREAMS limits server-initiated streams (push)
		case http2.SettingInitialWindowSize:
			if s.Val > 0x7fffffff {
				validationErr = fmt.Errorf("SETTINGS_INITIAL_WINDOW_SIZE too large: %d", s.Val)
				_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeFlowControl, []byte(validationErr.Error()))
				return validationErr
			}
			p.manager.mu.Lock()
			//nolint:gosec // G115: safe conversion, values validated <= 2^31-1 above
			oldWindowSize := int32(p.manager.initialWindowSize)
			//nolint:gosec // G115: safe conversion, values validated <= 2^31-1 above
			newWindowSize := int32(s.Val)
			delta := newWindowSize - oldWindowSize
			p.manager.initialWindowSize = s.Val

			for sid, stream := range p.manager.streams {
				stream.mu.Lock()
				if delta > 0 && stream.WindowSize > 2147483647-delta {
					stream.mu.Unlock()
					p.manager.mu.Unlock()
					validationErr = fmt.Errorf("stream %d window overflow", sid)
					_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeFlowControl, []byte(validationErr.Error()))
					return validationErr
				}
				stream.WindowSize += delta

				// If window became positive and there's buffered data, prepare to flush.
				if stream.WindowSize > 0 && stream.OutboundBuffer != nil && stream.OutboundBuffer.Len() > 0 {
					buffered := stream.OutboundBuffer.Bytes()
					sendLen := len(buffered)
					if int32(sendLen) > stream.WindowSize {
						sendLen = int(stream.WindowSize)
					}
					isEnd := sendLen == len(buffered) && stream.OutboundEndStream
					data := make([]byte, sendLen)
					copy(data, buffered[:sendLen])
					stream.WindowSize -= int32(sendLen)

					if sendLen == len(buffered) {
						stream.OutboundBuffer.Reset()
					} else {
						remaining := make([]byte, len(buffered)-sendLen)
						copy(remaining, buffered[sendLen:])
						stream.OutboundBuffer.Reset()
						stream.OutboundBuffer.Write(remaining)
					}

					pendingFlushes = append(pendingFlushes, bufferedFlush{
						streamID:  sid,
						data:      data,
						endStream: isEnd,
					})
				}
				stream.mu.Unlock()
			}
			p.manager.mu.Unlock()
		case http2.SettingMaxFrameSize:
			if s.Val < 16384 {
				validationErr = fmt.Errorf("SETTINGS_MAX_FRAME_SIZE too small: %d", s.Val)
				return validationErr
			}
			if s.Val > 16777215 {
				validationErr = fmt.Errorf("SETTINGS_MAX_FRAME_SIZE too large: %d", s.Val)
				return validationErr
			}
			p.manager.mu.Lock()
			atomic.StoreUint32(&p.manager.maxFrameSize, s.Val)
			p.manager.mu.Unlock()
		case http2.SettingMaxHeaderListSize:
			// No specific validation
		}
		return nil
	})

	if validationErr != nil {
		return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
			[]byte(validationErr.Error()), validationErr)
	}

	// Send SETTINGS_ACK before flushing any pending DATA frames.
	// Peers expect the ACK to confirm settings are applied before
	// seeing frames that depend on the new settings.
	if err := p.writer.WriteSettingsAck(); err != nil {
		return err
	}
	p.flush()

	for _, pf := range pendingFlushes {
		_ = p.writer.WriteData(pf.streamID, pf.endStream, pf.data)
		p.flush()
	}

	return nil
}

// runHandler runs the stream handler synchronously.
// Running synchronously ensures response data is written to the output buffer
// before ProcessFrame returns, so the caller can flush it to the wire.
func (p *Processor) runHandler(stream *Stream) {
	if p.handler == nil {
		return
	}

	select {
	case <-stream.ctx.Done():
		return
	default:
	}

	stream.mu.Lock()
	stream.HandlerStarted = true
	stream.mu.Unlock()

	if err := p.handler.HandleStream(stream.ctx, stream); err != nil {
		select {
		case <-stream.ctx.Done():
			return
		default:
			_ = p.writer.WriteRSTStream(stream.ID, http2.ErrCodeInternal)
			p.flush()
		}
	} else {
		stream.mu.RLock()
		headersSent := stream.HeadersSent
		state := stream.State
		stream.mu.RUnlock()

		if !headersSent {
			if stream.ResponseWriter != nil {
				_ = stream.ResponseWriter.WriteResponse(stream, 200, nil, nil)
			}
			return
		}

		if state == StateOpen || state == StateHalfClosedRemote {
			if stream.OutboundBuffer != nil && stream.OutboundBuffer.Len() > 0 {
				// Data still buffered (flow control); will be flushed on WINDOW_UPDATE.
				return
			}

			// WriteResponse already sent DATA with END_STREAM, so just
			// transition the stream state without sending a duplicate frame.
			if state == StateHalfClosedRemote {
				stream.SetState(StateClosed)
			} else {
				stream.SetState(StateHalfClosedLocal)
			}
		}
	}
}

// handleHeaders processes HEADERS frames.
//
//nolint:gocyclo // complex header block assembly and stream lifecycle logic
func (p *Processor) handleHeaders(_ context.Context, f *http2.HeadersFrame) error {
	existingStream, exists := p.manager.GetStream(f.StreamID)

	if !f.HeadersEnded() {
		headerBlock := f.HeaderBlockFragment()
		frag := make([]byte, len(headerBlock))
		copy(frag, headerBlock)
		p.continuationStateMu.Lock()
		p.continuationState = &ContinuationState{
			streamID:      f.StreamID,
			headerBlock:   frag,
			endStream:     f.StreamEnded(),
			expectingMore: true,
			isTrailers:    false,
		}
		p.continuationStateMu.Unlock()
		return nil
	}

	if !exists {
		p.manager.mu.RLock()
		lastClientStream := p.manager.lastClientStream
		p.manager.mu.RUnlock()

		if f.StreamID <= lastClientStream {
			_ = p.SendGoAway(lastClientStream, http2.ErrCodeProtocol, []byte("HEADERS on closed stream (reused id)"))
			return fmt.Errorf("HEADERS frame on closed stream %d (last stream: %d)", f.StreamID, lastClientStream)
		}

		if err := validateStreamID(f.StreamID, lastClientStream, false); err != nil {
			return p.goAwayErr(lastClientStream, http2.ErrCodeProtocol, []byte(err.Error()), err)
		}

		p.manager.mu.Lock()
		if f.StreamID > p.manager.lastClientStream {
			p.manager.lastClientStream = f.StreamID
		}
		p.manager.mu.Unlock()
	}

	if exists {
		state := existingStream.GetState()
		switch state {
		case StateClosed:
			_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on closed stream (state closed)"))
			return fmt.Errorf("HEADERS frame on closed stream %d", f.StreamID)
		case StateHalfClosedRemote:
			_ = p.SendGoAway(p.manager.GetLastStreamID(), http2.ErrCodeStreamClosed, []byte("HEADERS on half-closed stream"))
			return fmt.Errorf("HEADERS frame on half-closed (remote) stream %d", f.StreamID)
		}

		if existingStream.ReceivedInitialHeaders {
			if !f.StreamEnded() {
				return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("second HEADERS without END_STREAM"),
					fmt.Errorf("second HEADERS without END_STREAM on stream %d", f.StreamID))
			}
			headerBlock := f.HeaderBlockFragment()
			if !f.HeadersEnded() {
				frag := make([]byte, len(headerBlock))
				copy(frag, headerBlock)
				p.continuationStateMu.Lock()
				p.continuationState = &ContinuationState{
					streamID:      f.StreamID,
					headerBlock:   frag,
					endStream:     f.StreamEnded(),
					expectingMore: true,
					isTrailers:    true,
				}
				p.continuationStateMu.Unlock()
				return nil
			}

			pooledTrailers := headersSlicePoolIn.Get().(*[][2]string)
			trailers := (*pooledTrailers)[:0]
			defer func() {
				*pooledTrailers = trailers[:0]
				headersSlicePoolIn.Put(pooledTrailers)
			}()
			p.hpackDecoder.SetEmitFunc(func(hf hpack.HeaderField) {
				trailers = append(trailers, [2]string{hf.Name, hf.Value})
			})
			if _, err := p.hpackDecoder.Write(headerBlock); err != nil {
				return p.goAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
					fmt.Errorf("failed to decode trailers: %w", err))
			}
			if err := p.hpackDecoder.Close(); err != nil {
				return p.goAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
					fmt.Errorf("failed to finalize trailers: %w", err))
			}
			if err := validateTrailerHeaders(trailers); err != nil {
				_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid trailers: %w", err)
			}
			for _, h := range trailers {
				existingStream.AddHeader(h[0], h[1])
			}

			if f.StreamEnded() {
				existingStream.EndStream = true
				existingStream.SetState(StateHalfClosedRemote)
				p.runHandler(existingStream)
			}
			return nil
		}
	}

	stream, ok := p.manager.TryOpenStream(f.StreamID)
	if !ok {
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeRefusedStream)
		return fmt.Errorf("exceeds MAX_CONCURRENT_STREAMS")
	}

	if err := validateStreamState(stream.GetState(), http2.FrameHeaders, f.StreamEnded()); err != nil {
		sendStreamError(p.writer, f.StreamID, http2.ErrCodeStreamClosed)
		return err
	}

	if stream.ResponseWriter == nil {
		stream.ResponseWriter = p.connWriter
	}

	headerBlock := f.HeaderBlockFragment()

	if dependency, weight, exclusive, hasPriority := ParsePriorityFromHeaders(f); hasPriority {
		if dependency == f.StreamID {
			return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("stream depends on itself"),
				fmt.Errorf("stream %d depends on itself", f.StreamID))
		}
		p.manager.priorityTree.UpdateFromFrame(f.StreamID, dependency, weight, exclusive)
	}

	if !f.HeadersEnded() {
		pooled := headerBlockPool.Get().(*[]byte)
		frag := (*pooled)[:0]
		frag = append(frag, headerBlock...)
		p.continuationStateMu.Lock()
		p.continuationState = &ContinuationState{
			streamID:      f.StreamID,
			headerBlock:   frag,
			endStream:     f.StreamEnded(),
			expectingMore: true,
			isTrailers:    false,
		}
		p.continuationStateMu.Unlock()
		stream.SetState(StateOpen)
		if stream.ResponseWriter == nil {
			stream.ResponseWriter = p.connWriter
		}
		return nil
	}

	pooledHeadersIn := headersSlicePoolIn.Get().(*[][2]string)
	headers := (*pooledHeadersIn)[:0]
	p.hpackDecoder.SetEmitFunc(func(hf hpack.HeaderField) {
		headers = append(headers, [2]string{hf.Name, hf.Value})
	})
	if _, err := p.hpackDecoder.Write(headerBlock); err != nil {
		return p.goAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to decode headers: %w", err))
	}
	if err := p.hpackDecoder.Close(); err != nil {
		return p.goAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
			fmt.Errorf("failed to finalize headers: %w", err))
	}

	if err := validateRequestHeaders(headers); err != nil {
		sendStreamError(p.writer, f.StreamID, http2.ErrCodeProtocol)
		return fmt.Errorf("invalid headers: %w", err)
	}
	for _, h := range headers {
		stream.AddHeader(h[0], h[1])
	}
	stream.ReceivedInitialHeaders = true

	if f.StreamEnded() {
		stream.EndStream = true
		stream.SetState(StateHalfClosedRemote)

		if err := validateContentLength(stream.Headers, stream.ReceivedDataLen); err != nil {
			sendStreamError(p.writer, f.StreamID, http2.ErrCodeProtocol)
			return fmt.Errorf("content-length mismatch: %w", err)
		}

		p.runHandler(stream)
	}

	return nil
}

// handleData processes DATA frames.
func (p *Processor) handleData(_ context.Context, f *http2.DataFrame) error {
	stream, ok := p.manager.GetStream(f.StreamID)
	if !ok {
		return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("DATA on idle stream"),
			fmt.Errorf("DATA frame on idle stream %d", f.StreamID))
	}

	state := stream.GetState()
	if state == StateClosed {
		_ = p.writer.WriteRSTStream(f.StreamID, http2.ErrCodeStreamClosed)
		p.flush()
		return fmt.Errorf("DATA frame on closed stream %d", f.StreamID)
	}

	if err := validateStreamState(state, http2.FrameData, f.StreamEnded()); err != nil {
		_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeStreamClosed)
		return err
	}

	dataLen := len(f.Data())
	stream.ReceivedDataLen += dataLen

	if err := stream.AddData(f.Data()); err != nil {
		return err
	}

	//nolint:gosec // G115: safe conversion, dataLen is frame payload size
	updateLen := uint32(dataLen)

	p.manager.AccumulateWindowUpdate(f.StreamID, updateLen)
	p.manager.AccumulateWindowUpdate(0, updateLen)

	if f.StreamEnded() {
		p.manager.FlushWindowUpdates(p.writer, true)
		stream.EndStream = true
		stream.SetState(StateHalfClosedRemote)

		if err := validateContentLength(stream.Headers, stream.ReceivedDataLen); err != nil {
			sendStreamError(p.writer, stream.ID, http2.ErrCodeProtocol)
			return fmt.Errorf("content-length mismatch: %w", err)
		}

		p.runHandler(stream)
	} else {
		p.manager.FlushWindowUpdates(p.writer, false)
	}

	return nil
}

// handleWindowUpdate processes WINDOW_UPDATE frames.
func (p *Processor) handleWindowUpdate(f *http2.WindowUpdateFrame) error {
	if f.Increment == 0 {
		if f.StreamID == 0 {
			return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("WINDOW_UPDATE increment is 0"),
				fmt.Errorf("WINDOW_UPDATE with 0 increment"))
		}
		return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("WINDOW_UPDATE with 0 increment on stream"),
			fmt.Errorf("WINDOW_UPDATE with 0 increment on stream %d", f.StreamID))
	}

	if f.StreamID == 0 {
		for {
			oldWin := atomic.LoadInt32(&p.manager.connectionWindow)
			newWin := int64(oldWin) + int64(f.Increment)
			if newWin > 0x7fffffff {
				return p.goAwayErr(0, http2.ErrCodeFlowControl, []byte("connection window overflow"),
					fmt.Errorf("connection window overflow: %d + %d > 2^31-1", oldWin, f.Increment))
			}
			//nolint:gosec // G115: safe conversion, newWin validated above
			if atomic.CompareAndSwapInt32(&p.manager.connectionWindow, oldWin, int32(newWin)) {
				break
			}
		}
	} else {
		stream, ok := p.manager.GetStream(f.StreamID)
		if !ok {
			if f.StreamID <= p.manager.GetLastClientStreamID() {
				return nil
			}
			return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
				[]byte("WINDOW_UPDATE on idle stream"),
				fmt.Errorf("WINDOW_UPDATE on idle stream %d", f.StreamID))
		}

		if stream.GetState() == StateClosed {
			return nil
		}

		stream.mu.Lock()
		newWindow := int64(stream.WindowSize) + int64(f.Increment)
		if newWindow > 0x7fffffff {
			stream.mu.Unlock()
			_ = p.writer.WriteRSTStream(f.StreamID, http2.ErrCodeFlowControl)
			p.flush()
			return fmt.Errorf("stream %d window overflow: %d + %d > 2^31-1", f.StreamID, stream.WindowSize, f.Increment)
		}
		//nolint:gosec // G115: safe conversion, newWindow validated <= 2^31-1 above
		stream.WindowSize = int32(newWindow)

		// Flush buffered outbound data now that window space is available.
		if stream.OutboundBuffer != nil && stream.OutboundBuffer.Len() > 0 && stream.WindowSize > 0 {
			buffered := stream.OutboundBuffer.Bytes()
			sendLen := len(buffered)
			if int32(sendLen) > stream.WindowSize {
				sendLen = int(stream.WindowSize)
			}
			isEnd := sendLen == len(buffered) && stream.OutboundEndStream
			stream.WindowSize -= int32(sendLen)
			stream.mu.Unlock()

			_ = p.writer.WriteData(f.StreamID, isEnd, buffered[:sendLen])
			p.flush()

			stream.mu.Lock()
			if sendLen == len(buffered) {
				stream.OutboundBuffer.Reset()
			} else {
				remaining := make([]byte, len(buffered)-sendLen)
				copy(remaining, buffered[sendLen:])
				stream.OutboundBuffer.Reset()
				stream.OutboundBuffer.Write(remaining)
			}
			stream.mu.Unlock()
		} else {
			stream.mu.Unlock()
		}

		select {
		//nolint:gosec // G115: safe conversion
		case stream.ReceivedWindowUpd <- int32(f.Increment):
		default:
		}
	}
	return nil
}

// handleRSTStream processes RST_STREAM frames.
func (p *Processor) handleRSTStream(f *http2.RSTStreamFrame) error {
	stream, ok := p.manager.GetStream(f.StreamID)
	if !ok {
		return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol,
			[]byte("RST_STREAM on idle stream"),
			fmt.Errorf("RST_STREAM on idle stream %d", f.StreamID))
	}

	stream.SetState(StateClosed)
	stream.mu.Lock()
	stream.ClosedByReset = true
	stream.mu.Unlock()
	if stream.cancel != nil {
		stream.cancel()
	}

	return nil
}

// handlePriority processes PRIORITY frames.
func (p *Processor) handlePriority(f *http2.PriorityFrame) error {
	if f.StreamDep == f.StreamID {
		return p.goAwayErr(p.manager.GetLastStreamID(), http2.ErrCodeProtocol, []byte("stream depends on itself"),
			fmt.Errorf("stream %d depends on itself", f.StreamID))
	}

	_, exists := p.manager.GetStream(f.StreamID)
	if !exists {
		stream := p.manager.GetOrCreateStream(f.StreamID)
		stream.SetState(StateIdle)
	}

	p.manager.priorityTree.UpdateFromFrame(
		f.StreamID,
		f.StreamDep,
		f.Weight,
		f.Exclusive,
	)
	return nil
}

// handleGoAway processes GOAWAY frames.
func (p *Processor) handleGoAway(f *http2.GoAwayFrame) error {
	lastStreamID := f.LastStreamID

	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()

	for streamID, stream := range p.manager.streams {
		if streamID > lastStreamID {
			stream.mu.Lock()
			prev := stream.State
			stream.State = StateClosed
			stream.mu.Unlock()
			p.manager.markActiveTransition(prev, StateClosed)
			delete(p.manager.streams, streamID)
		}
	}

	return nil
}

// handlePing processes PING frames.
func (p *Processor) handlePing(f *http2.PingFrame) error {
	if f.Header().StreamID != 0 {
		return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("PING on non-zero stream"),
			fmt.Errorf("PING frame with non-zero stream id: %d", f.Header().StreamID))
	}

	if !f.IsAck() {
		if err := p.writer.WritePing(true, f.Data); err != nil {
			return err
		}
		p.flush()
	}
	return nil
}

// handleContinuation processes CONTINUATION frames.
func (p *Processor) handleContinuation(_ context.Context, f *http2.ContinuationFrame) error {
	p.continuationStateMu.Lock()
	defer p.continuationStateMu.Unlock()

	if p.continuationState == nil || !p.continuationState.expectingMore {
		return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("unexpected CONTINUATION"),
			fmt.Errorf("unexpected CONTINUATION frame on stream %d", f.StreamID))
	}

	if p.continuationState.streamID != f.StreamID {
		return p.goAwayErr(0, http2.ErrCodeProtocol, []byte("CONTINUATION on wrong stream"),
			fmt.Errorf("CONTINUATION frame on wrong stream: expected %d, got %d",
				p.continuationState.streamID, f.StreamID))
	}

	p.continuationState.headerBlock = append(
		p.continuationState.headerBlock,
		f.HeaderBlockFragment()...,
	)

	if f.HeadersEnded() {
		stream := p.manager.GetOrCreateStream(f.StreamID)
		stream.SetState(StateOpen)

		pooledHeadersIn := headersSlicePoolIn.Get().(*[][2]string)
		headers := (*pooledHeadersIn)[:0]
		defer func() {
			*pooledHeadersIn = headers[:0]
			headersSlicePoolIn.Put(pooledHeadersIn)
		}()
		p.hpackDecoder.SetEmitFunc(func(hf hpack.HeaderField) {
			headers = append(headers, [2]string{hf.Name, hf.Value})
		})

		if _, err := p.hpackDecoder.Write(p.continuationState.headerBlock); err != nil {
			p.continuationState = nil
			return p.goAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
				fmt.Errorf("failed to decode headers: %w", err))
		}
		if err := p.hpackDecoder.Close(); err != nil {
			p.continuationState = nil
			return p.goAwayErr(0, http2.ErrCodeCompression, []byte("HPACK decoding failed"),
				fmt.Errorf("failed to finalize headers: %w", err))
		}

		if p.continuationState.isTrailers {
			if err := validateTrailerHeaders(headers); err != nil {
				p.continuationState = nil
				_ = p.sendRSTStreamAndMarkClosed(f.StreamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid trailers: %w", err)
			}
			for _, h := range headers {
				stream.AddHeader(h[0], h[1])
			}
		} else {
			if err := validateRequestHeaders(headers); err != nil {
				p.continuationState = nil
				sendStreamError(p.writer, f.StreamID, http2.ErrCodeProtocol)
				return fmt.Errorf("invalid headers: %w", err)
			}
			for _, h := range headers {
				stream.AddHeader(h[0], h[1])
			}
			stream.ReceivedInitialHeaders = true
			if stream.ResponseWriter == nil {
				stream.ResponseWriter = p.connWriter
			}
		}

		if p.continuationState.endStream {
			stream.EndStream = true
			stream.SetState(StateHalfClosedRemote)
			p.runHandler(stream)
		}

		if p.continuationState != nil {
			b := p.continuationState.headerBlock
			pooled := b[:0]
			headerBlockPool.Put(&pooled)
		}
		p.continuationState = nil
	}

	return nil
}

// flush flushes the writer if it supports the Flush method.
func (p *Processor) flush() {
	if flusher, ok := p.writer.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

// goAwayErr sends a GOAWAY frame and returns the given error.
// SendGoAway handles flushing internally.
func (p *Processor) goAwayErr(lastStreamID uint32, code http2.ErrCode, debug []byte, err error) error {
	_ = p.SendGoAway(lastStreamID, code, debug)
	return err
}

// SendGoAway sends a GOAWAY frame.
func (p *Processor) SendGoAway(lastStreamID uint32, code http2.ErrCode, debugData []byte) error {
	if p.connWriter != nil {
		return p.connWriter.SendGoAway(lastStreamID, code, debugData)
	}
	if err := p.writer.WriteGoAway(lastStreamID, code, debugData); err != nil {
		return err
	}
	p.flush()
	return nil
}

// sendRSTStreamAndMarkClosed sends RST_STREAM and marks the stream as closed.
func (p *Processor) sendRSTStreamAndMarkClosed(streamID uint32, code http2.ErrCode) error {
	stream, ok := p.manager.GetStream(streamID)

	if ok && stream.cancel != nil {
		stream.cancel()
	}

	if p.connWriter != nil {
		p.connWriter.MarkStreamClosed(streamID)
	}

	if p.connWriter != nil {
		if err := p.connWriter.WriteRSTStreamPriority(streamID, code); err != nil {
			return err
		}
	} else {
		if err := p.writer.WriteRSTStream(streamID, code); err != nil {
			return err
		}
		p.flush()
	}

	if s, ok := p.manager.GetStream(streamID); ok {
		s.mu.Lock()
		s.HeadersSent = false
		s.IsStreaming = false
		s.OutboundEndStream = false
		if s.OutboundBuffer != nil {
			s.OutboundBuffer.Reset()
		}
		s.OutboundBuffer = nil
		s.phase = PhaseHeadersSent
		s.mu.Unlock()
	}

	return nil
}

// FlushBufferedData exposes flushBufferedData for external callers.
func (p *Processor) FlushBufferedData(_ uint32) {
	// Placeholder for transport-layer integration
}

// GetStreamPriority returns the priority score for a stream.
func (p *Processor) GetStreamPriority(streamID uint32) int {
	return p.manager.priorityTree.CalculateStreamPriority(streamID)
}
