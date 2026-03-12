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

// H1State holds per-connection H1 parsing state.
type H1State struct {
	parser     *h1.Parser
	buffer     bytes.Buffer
	req        h1.Request
	RemoteAddr string
	HijackFn   func() (net.Conn, error) // set by engine; nil if unsupported
}

// NewH1State creates a new H1 connection state.
func NewH1State() *H1State {
	return &H1State{
		parser: h1.NewParser(),
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
				write(buildErrorResponse(400, "Bad Request"))
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
				state.buffer.Write(data[offset:])
				break
			}

			if err := handleH1Request(ctx, &state.req, nil, state.RemoteAddr, handler, write, state.HijackFn); err != nil {
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
			write(buildErrorResponse(400, "Bad Request"))
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
					write(buildErrorResponse(400, "Invalid chunked encoding"))
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
					write(buildErrorResponse(413, "Request body too large"))
					return fmt.Errorf("chunked body exceeds %d byte limit", maxRequestBodySize)
				}
			}
			bodyData = chunks.Bytes()
		default:
			state.buffer.Next(consumed)
		}

		if err := handleH1Request(ctx, &state.req, bodyData, state.RemoteAddr, handler, write, state.HijackFn); err != nil {
			return err
		}
		if !state.req.KeepAlive {
			return fmt.Errorf("connection close requested")
		}
	}
	return nil
}

func handleH1Request(ctx context.Context, req *h1.Request, body []byte, remoteAddr string,
	handler stream.Handler, write func([]byte), hijackFn func() (net.Conn, error)) error {

	s := requestToStream(req, body, remoteAddr)
	defer s.Release()
	rw := &h1ResponseAdapter{
		write: write, keepAlive: req.KeepAlive,
		isHEAD: req.Method == "HEAD", hijackFn: hijackFn,
	}
	s.ResponseWriter = rw

	if err := handler.HandleStream(ctx, s); err != nil {
		if rw.hijacked {
			return ErrHijacked
		}
		write(buildErrorResponse(500, "Internal Server Error"))
		return err
	}
	if rw.hijacked {
		return ErrHijacked
	}
	return nil
}

func requestToStream(req *h1.Request, body []byte, remoteAddr string) *stream.Stream {
	s := stream.NewStream(1)
	s.RemoteAddr = remoteAddr
	// Reuse the stream's existing header slice capacity from the pool.
	hdrs := s.Headers[:0]
	needed := len(req.Headers) + 4
	if cap(hdrs) < needed {
		hdrs = make([][2]string, 0, needed)
	}
	hdrs = append(hdrs,
		[2]string{":method", req.Method},
		[2]string{":path", req.Path},
		[2]string{":scheme", "http"},
		[2]string{":authority", req.Host},
	)
	hdrs = append(hdrs, req.Headers...)
	s.Headers = hdrs

	if len(body) > 0 {
		_, _ = s.Data.Write(body)
	}
	s.EndStream = true
	s.SetState(stream.StateHalfClosedRemote)
	return s
}
