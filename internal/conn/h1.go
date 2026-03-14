// Package conn provides shared HTTP/1.1 and HTTP/2 connection handling.
package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"

	h1 "github.com/goceleris/celeris/protocol/h1"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// ErrHijacked is returned by ProcessH1 when the connection was hijacked.
// The engine must not close or reuse the FD after receiving this error.
var ErrHijacked = errors.New("celeris: connection hijacked")

// maxRequestBodySize is the maximum allowed request body (100 MB), matching H2.
const maxRequestBodySize = 100 << 20

// continue100Response is sent when the client sends "Expect: 100-continue"
// to signal that the server is willing to accept the request body.
var continue100Response = []byte("HTTP/1.1 100 Continue\r\n\r\n")

// H1State holds per-connection H1 parsing state.
type H1State struct {
	parser     *h1.Parser
	buffer     bytes.Buffer
	req        h1.Request
	rw         h1ResponseAdapter // embedded — reused per request, avoids heap alloc
	RemoteAddr string
	HijackFn   func() (net.Conn, error) // set by engine; nil if unsupported
}

// NewH1State creates a new H1 connection state with zero-copy header parsing.
func NewH1State() *H1State {
	p := h1.NewParser()
	p.SetZeroCopy(true)
	return &H1State{
		parser: p,
	}
}

// ProcessH1 processes incoming H1 data, parsing requests and calling the handler.
// The write callback is used to send response bytes back to the connection.
func ProcessH1(ctx context.Context, data []byte, state *H1State, handler stream.Handler,
	write func([]byte)) error {

	if state.buffer.Len() == 0 {
		offset := 0
		for offset < len(data) {
			state.parser.Reset(data[offset:])
			state.req.Reset()
			consumed, err := state.parser.ParseRequest(&state.req)
			if err != nil {
				writeErrorResponse(write, 400, "Bad Request")
				return err
			}
			if consumed == 0 {
				state.buffer.Write(data[offset:])
				return nil
			}

			bodyNeeded := int64(0)
			if state.req.ChunkedEncoding {
				bodyNeeded = -1
			} else if state.req.ContentLength > 0 {
				bodyNeeded = state.req.ContentLength
			}

			if bodyNeeded > 0 || bodyNeeded == -1 {
				if state.req.ExpectContinue {
					write(continue100Response)
					state.req.ExpectContinue = false
				}
				state.buffer.Write(data[offset:])
				break
			}

			if err := handleH1Request(ctx, state, nil, handler, write); err != nil {
				return err
			}
			if !state.req.KeepAlive {
				return fmt.Errorf("connection close requested")
			}
			offset += consumed
		}
	} else {
		state.buffer.Write(data)
	}

	// Buffered path
	for state.buffer.Len() > 0 {
		state.parser.Reset(state.buffer.Bytes())
		state.req.Reset()
		consumed, err := state.parser.ParseRequest(&state.req)
		if err != nil {
			writeErrorResponse(write, 400, "Bad Request")
			return err
		}
		if consumed == 0 {
			break
		}

		bodyNeeded := int64(0)
		if state.req.ChunkedEncoding {
			bodyNeeded = -1
		} else if state.req.ContentLength > 0 {
			bodyNeeded = state.req.ContentLength
		}

		if state.req.ExpectContinue && (bodyNeeded > 0 || bodyNeeded == -1) {
			write(continue100Response)
			state.req.ExpectContinue = false
		}

		var bodyData []byte
		switch {
		case bodyNeeded > 0:
			available := int64(state.buffer.Len() - consumed)
			if available < bodyNeeded {
				return nil
			}
			state.buffer.Next(consumed)
			buf := state.buffer.Bytes()
			bodyData = buf[:bodyNeeded]
			state.buffer.Next(int(bodyNeeded))
		case bodyNeeded == -1:
			state.buffer.Next(consumed)
			var chunks bytes.Buffer
			for {
				state.parser.Reset(state.buffer.Bytes())
				chunk, chunkConsumed, cerr := state.parser.ParseChunkedBody()
				if cerr != nil {
					writeErrorResponse(write, 400, "Invalid chunked encoding")
					return cerr
				}
				if chunkConsumed == 0 {
					return nil
				}
				state.buffer.Next(chunkConsumed)
				if chunk == nil {
					break
				}
				chunks.Write(chunk)
				if chunks.Len() > maxRequestBodySize {
					writeErrorResponse(write, 413, "Request body too large")
					return fmt.Errorf("chunked body exceeds %d byte limit", maxRequestBodySize)
				}
			}
			bodyData = chunks.Bytes()
		default:
			state.buffer.Next(consumed)
		}

		if err := handleH1Request(ctx, state, bodyData, handler, write); err != nil {
			return err
		}
		if !state.req.KeepAlive {
			return fmt.Errorf("connection close requested")
		}
	}
	return nil
}

func handleH1Request(ctx context.Context, state *H1State, body []byte,
	handler stream.Handler, write func([]byte)) error {

	req := &state.req
	s := requestToStream(req, body, state.RemoteAddr)
	defer s.Release()

	// Reuse the connection-scoped response adapter — avoids a heap allocation
	// per request. Reset per-request fields; hijackFn/write are stable.
	rw := &state.rw
	rw.write = write
	rw.keepAlive = req.KeepAlive
	rw.isHEAD = req.Method == "HEAD"
	rw.hijackFn = state.HijackFn
	rw.hijacked = false
	s.ResponseWriter = rw

	if err := handler.HandleStream(ctx, s); err != nil {
		if rw.hijacked {
			return ErrHijacked
		}
		writeErrorResponse(write, 500, "Internal Server Error")
		return err
	}
	if rw.hijacked {
		return ErrHijacked
	}
	return nil
}

func requestToStream(req *h1.Request, body []byte, remoteAddr string) *stream.Stream {
	s := stream.NewH1Stream(1)
	s.RemoteAddr = remoteAddr
	// Reuse the stream's existing header slice capacity from the pool.
	hdrs := s.Headers[:0]
	needed := len(req.RawHeaders) + 4
	if cap(hdrs) < needed {
		hdrs = make([][2]string, 0, needed)
	}
	hdrs = append(hdrs,
		[2]string{":method", req.Method},
		[2]string{":path", req.Path},
		[2]string{":scheme", "http"},
		[2]string{":authority", req.Host},
	)
	// Zero-copy header conversion: lowercase names in-place, then create
	// strings backed by the read buffer. Safe because H1 handlers run
	// synchronously — the buffer isn't reused until after the stream is released.
	for _, rh := range req.RawHeaders {
		hdrs = append(hdrs, [2]string{
			h1.UnsafeLowerHeader(rh[0]),
			h1.UnsafeString(rh[1]),
		})
	}
	s.Headers = hdrs
	s.IsHEAD = req.Method == "HEAD"

	if len(body) > 0 {
		_, _ = s.GetBuf().Write(body)
	}
	s.EndStream = true
	s.SetState(stream.StateHalfClosedRemote)
	return s
}
