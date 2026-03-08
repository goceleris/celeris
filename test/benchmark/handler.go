package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

type benchHandler struct{}

func (h *benchHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	headers := s.GetHeaders()
	for _, hdr := range headers {
		if hdr[0] == ":path" {
			path = hdr[1]
			break
		}
	}

	switch {
	case path == "/":
		return h.plaintext(s)
	case path == "/json":
		return h.jsonResponse(s)
	case len(path) > 7 && path[:7] == "/users/":
		return h.pathParam(s, path[7:])
	case path == "/upload":
		return h.upload(s)
	default:
		return h.plaintext(s)
	}
}

func (h *benchHandler) plaintext(s *stream.Stream) error {
	body := []byte("Hello, World!")
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "text/plain"},
			{"content-length", strconv.Itoa(len(body))},
		}, body)
	}
	return nil
}

func (h *benchHandler) jsonResponse(s *stream.Stream) error {
	data, _ := json.Marshal(map[string]string{"message": "Hello, World!"})
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "application/json"},
			{"content-length", strconv.Itoa(len(data))},
		}, data)
	}
	return nil
}

func (h *benchHandler) pathParam(s *stream.Stream, id string) error {
	data, _ := json.Marshal(map[string]string{"id": id})
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "application/json"},
			{"content-length", strconv.Itoa(len(data))},
		}, data)
	}
	return nil
}

func (h *benchHandler) upload(s *stream.Stream) error {
	body := s.GetData()
	resp := []byte(fmt.Sprintf("%d", len(body)))
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "text/plain"},
			{"content-length", strconv.Itoa(len(resp))},
		}, resp)
	}
	return nil
}
