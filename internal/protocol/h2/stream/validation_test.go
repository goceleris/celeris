package stream

import (
	"testing"

	"golang.org/x/net/http2"
)

func TestValidateRequestHeaders(t *testing.T) {
	// Valid request
	valid := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/"},
		{":authority", "example.com"},
		{"content-type", "text/html"},
	}
	if err := validateRequestHeaders(valid); err != nil {
		t.Errorf("Valid headers rejected: %v", err)
	}
}

func TestValidateRequestHeadersMissingMethod(t *testing.T) {
	headers := [][2]string{
		{":scheme", "https"},
		{":path", "/"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for missing :method")
	}
}

func TestValidateRequestHeadersMissingScheme(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{":path", "/"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for missing :scheme")
	}
}

func TestValidateRequestHeadersMissingPath(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for missing :path")
	}
}

func TestValidateRequestHeadersEmptyPath(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", ""},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for empty :path")
	}
}

func TestValidateRequestHeadersPseudoAfterRegular(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{"content-type", "text/html"},
		{":scheme", "https"},
		{":path", "/"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for pseudo-header after regular header")
	}
}

func TestValidateRequestHeadersDuplicatePseudo(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{":method", "POST"},
		{":scheme", "https"},
		{":path", "/"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for duplicate pseudo-header")
	}
}

func TestValidateRequestHeadersUnknownPseudo(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/"},
		{":unknown", "value"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for unknown pseudo-header")
	}
}

func TestValidateRequestHeadersConnectionSpecific(t *testing.T) {
	forbidden := []string{"connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade"}
	for _, name := range forbidden {
		headers := [][2]string{
			{":method", "GET"},
			{":scheme", "https"},
			{":path", "/"},
			{name, "value"},
		}
		if err := validateRequestHeaders(headers); err == nil {
			t.Errorf("Expected error for connection-specific header %q", name)
		}
	}
}

func TestValidateRequestHeadersTETrailers(t *testing.T) {
	// te: trailers is allowed
	headers := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/"},
		{"te", "trailers"},
	}
	if err := validateRequestHeaders(headers); err != nil {
		t.Errorf("te:trailers should be allowed: %v", err)
	}

	// te: other values are not allowed
	headers = [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/"},
		{"te", "chunked"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for te:chunked")
	}
}

func TestValidateRequestHeadersUppercase(t *testing.T) {
	headers := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/"},
		{"Content-Type", "text/html"},
	}
	if err := validateRequestHeaders(headers); err == nil {
		t.Error("Expected error for uppercase header name")
	}
}

func TestValidateTrailerHeaders(t *testing.T) {
	valid := [][2]string{
		{"x-checksum", "abc123"},
		{"grpc-status", "0"},
	}
	if err := validateTrailerHeaders(valid); err != nil {
		t.Errorf("Valid trailers rejected: %v", err)
	}
}

func TestValidateTrailerHeadersPseudo(t *testing.T) {
	trailers := [][2]string{
		{":status", "200"},
	}
	if err := validateTrailerHeaders(trailers); err == nil {
		t.Error("Expected error for pseudo-header in trailers")
	}
}

func TestValidateTrailerHeadersConnectionSpecific(t *testing.T) {
	trailers := [][2]string{
		{"connection", "close"},
	}
	if err := validateTrailerHeaders(trailers); err == nil {
		t.Error("Expected error for connection-specific header in trailers")
	}
}

func TestValidateTrailerHeadersUppercase(t *testing.T) {
	trailers := [][2]string{
		{"X-Checksum", "abc123"},
	}
	if err := validateTrailerHeaders(trailers); err == nil {
		t.Error("Expected error for uppercase trailer name")
	}
}

func TestValidateContentLength(t *testing.T) {
	headers := [][2]string{
		{"content-length", "5"},
	}
	if err := validateContentLength(headers, 5); err != nil {
		t.Errorf("Valid content-length rejected: %v", err)
	}
}

func TestValidateContentLengthMismatch(t *testing.T) {
	headers := [][2]string{
		{"content-length", "10"},
	}
	if err := validateContentLength(headers, 5); err == nil {
		t.Error("Expected error for content-length mismatch")
	}
}

func TestValidateContentLengthInvalid(t *testing.T) {
	headers := [][2]string{
		{"content-length", "abc"},
	}
	if err := validateContentLength(headers, 0); err == nil {
		t.Error("Expected error for invalid content-length")
	}
}

func TestValidateContentLengthMissing(t *testing.T) {
	headers := [][2]string{
		{"content-type", "text/html"},
	}
	if err := validateContentLength(headers, 100); err != nil {
		t.Errorf("Missing content-length should pass: %v", err)
	}
}

func TestValidateStreamID(t *testing.T) {
	if err := validateStreamID(0, 0, false); err == nil {
		t.Error("Expected error for stream ID 0")
	}

	if err := validateStreamID(1, 0, false); err != nil {
		t.Errorf("Valid stream ID rejected: %v", err)
	}

	if err := validateStreamID(2, 0, false); err == nil {
		t.Error("Expected error for even stream ID from server perspective")
	}

	if err := validateStreamID(1, 1, false); err == nil {
		t.Error("Expected error for stream ID not greater than last")
	}

	if err := validateStreamID(3, 1, false); err != nil {
		t.Errorf("Valid stream ID rejected: %v", err)
	}
}

func TestValidateStreamState(t *testing.T) {
	// Idle stream only allows HEADERS and PRIORITY
	if err := validateStreamState(StateIdle, http2.FrameHeaders, false); err != nil {
		t.Errorf("HEADERS on idle should be allowed: %v", err)
	}
	if err := validateStreamState(StateIdle, http2.FramePriority, false); err != nil {
		t.Errorf("PRIORITY on idle should be allowed: %v", err)
	}
	if err := validateStreamState(StateIdle, http2.FrameData, false); err == nil {
		t.Error("DATA on idle should be rejected")
	}

	// Half-closed (remote) rejects DATA and HEADERS
	if err := validateStreamState(StateHalfClosedRemote, http2.FrameData, false); err == nil {
		t.Error("DATA on half-closed remote should be rejected")
	}
	if err := validateStreamState(StateHalfClosedRemote, http2.FrameHeaders, false); err == nil {
		t.Error("HEADERS on half-closed remote should be rejected")
	}
	if err := validateStreamState(StateHalfClosedRemote, http2.FrameWindowUpdate, false); err != nil {
		t.Errorf("WINDOW_UPDATE on half-closed remote should be allowed: %v", err)
	}

	// Closed stream only allows PRIORITY, RST_STREAM, and WINDOW_UPDATE
	if err := validateStreamState(StateClosed, http2.FramePriority, false); err != nil {
		t.Errorf("PRIORITY on closed should be allowed: %v", err)
	}
	if err := validateStreamState(StateClosed, http2.FrameRSTStream, false); err != nil {
		t.Errorf("RST_STREAM on closed should be allowed: %v", err)
	}
	if err := validateStreamState(StateClosed, http2.FrameData, false); err == nil {
		t.Error("DATA on closed should be rejected")
	}

	// Open stream allows everything
	if err := validateStreamState(StateOpen, http2.FrameData, false); err != nil {
		t.Errorf("DATA on open should be allowed: %v", err)
	}
}
