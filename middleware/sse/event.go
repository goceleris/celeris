package sse

import (
	"errors"
	"strconv"
	"strings"
)

// ErrClientClosed is returned when Send is called on a closed Client.
var ErrClientClosed = errors.New("sse: client closed")

// Event represents a single Server-Sent Event.
type Event struct {
	// ID is the event ID. If non-empty, sent as "id: <ID>\n".
	// The client stores this and sends it back as Last-Event-ID on reconnect.
	ID string

	// Event is the event type. If non-empty, sent as "event: <Event>\n".
	// Defaults to "message" on the client side when omitted.
	Event string

	// Data is the event payload. Sent as "data: <line>\n" for each line.
	// Multi-line data is split on \n and each line gets its own "data:" prefix.
	Data string

	// Retry is the reconnection time in milliseconds. If > 0, sent as
	// "retry: <Retry>\n". Per the SSE specification, this value is in
	// milliseconds (not time.Duration) to match the wire format directly.
	Retry int
}

// FormatEvent formats an SSE event into buf, reusing its capacity.
// Exported for benchmarking; most users should use [Client.Send] instead.
func FormatEvent(buf []byte, e Event) []byte {
	return formatEvent(buf, &e)
}

// heartbeatBytes is a pre-allocated comment line used as a keep-alive.
var heartbeatBytes = []byte(": heartbeat\n\n")

// formatEvent writes the SSE wire format into buf, reusing its capacity.
// Returns the slice of buf containing the formatted event.
func formatEvent(buf []byte, e *Event) []byte {
	buf = buf[:0]

	if e.ID != "" {
		buf = appendField(buf, "id: ", e.ID)
	}
	if e.Event != "" {
		buf = appendField(buf, "event: ", e.Event)
	}
	if e.Retry > 0 {
		buf = append(buf, "retry: "...)
		buf = strconv.AppendInt(buf, int64(e.Retry), 10)
		buf = append(buf, '\n')
	}
	if e.Data != "" {
		buf = appendData(buf, e.Data)
	}
	buf = append(buf, '\n') // blank line terminates event
	return buf
}

// appendField writes a single-line SSE field, stripping \r, \n, and \0
// to prevent field injection. Used for id and event fields.
func appendField(buf []byte, prefix, value string) []byte {
	buf = append(buf, prefix...)
	for i := range len(value) {
		b := value[i]
		if b != '\r' && b != '\n' && b != 0 {
			buf = append(buf, b)
		}
	}
	buf = append(buf, '\n')
	return buf
}

// appendData writes "data: <line>\n" for each line in s.
// Handles \n, \r\n, and bare \r as line terminators per the SSE spec.
func appendData(buf []byte, s string) []byte {
	for {
		i := strings.IndexAny(s, "\r\n")
		if i < 0 {
			buf = append(buf, "data: "...)
			buf = append(buf, s...)
			buf = append(buf, '\n')
			return buf
		}
		buf = append(buf, "data: "...)
		buf = append(buf, s[:i]...)
		buf = append(buf, '\n')
		// Consume \r\n as a single line terminator.
		if s[i] == '\r' {
			if i+1 < len(s) && s[i+1] == '\n' {
				s = s[i+2:]
			} else {
				s = s[i+1:]
			}
		} else {
			s = s[i+1:]
		}
	}
}

// formatComment writes a comment line ": <text>\n\n" into buf.
func formatComment(buf []byte, text string) []byte {
	buf = buf[:0]
	buf = append(buf, ": "...)
	buf = append(buf, text...)
	buf = append(buf, '\n', '\n')
	return buf
}
