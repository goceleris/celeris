package sse

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"
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

// ownEvent returns e with its strings copied into one allocation, for an
// Event that is kept after the Send it was given to returns: on the
// per-client queue until the drain writes it, or in the ring replay store.
//
// A sender's strings can be request strings: a handler that relays
// c.Param or c.Query to a connected Client (celeris#732). On epoll and
// io_uring (and Adaptive, which runs them) those are views of the sending
// connection's receive buffer, which the engine reuses for that
// connection's next request and, once it closes, for another connection.
// A kept view would later read those bytes, and the drain or a replay would
// send them to this Client.
func ownEvent(e Event) Event {
	n := len(e.ID) + len(e.Event) + len(e.Data)
	if n == 0 {
		return e
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString(e.ID)
	b.WriteString(e.Event)
	b.WriteString(e.Data)
	s := b.String()
	e.ID, s = s[:len(e.ID)], s[len(e.ID):]
	e.Event, e.Data = s[:len(e.Event)], s[len(e.Event):]
	return e
}

// ownedCtx is the context the drain gives a ReplayStore other than the bare
// ring in place of the client's own: the same context, plus the ownedSlot of the
// event being appended (celeris#888 item 24). sendQueued copied the event's
// strings (ownEvent) before it queued it, so the ring can keep them as they are,
// also when another store wraps it and forwards the event with the context it was
// given: the ring tells the context by its type and keeps the event only if its
// strings are those very strings, by address and length. An event a decorator
// changed, one that never went through the queue, and a call made with another
// context (a decorator that derives its own) do not match: the ring copies, so
// nothing a caller can still change is kept. A type assertion, not
// Context.Value, so that Append on the blocking path pays nothing it can see.
type ownedCtx struct {
	context.Context
	ownedSlot
}

// ownedSlot records the strings of the event the drain is handing to the store.
// The drain is the only writer; the ring may read from any goroutine, hence the
// atomics. The slot holds the strings it names alive, and a stale slot names an
// event that really is owned, so a late Append of that event is still right.
type ownedSlot struct {
	event, data       atomic.Pointer[byte]
	eventLen, dataLen atomic.Int64
}

func (s *ownedSlot) set(e *Event) {
	s.event.Store(unsafe.StringData(e.Event))
	s.eventLen.Store(int64(len(e.Event)))
	s.data.Store(unsafe.StringData(e.Data))
	s.dataLen.Store(int64(len(e.Data)))
}

func sameString(p *atomic.Pointer[byte], n *atomic.Int64, s string) bool {
	return int64(len(s)) == n.Load() && (len(s) == 0 || p.Load() == unsafe.StringData(s))
}

// owns reports that e's Event and Data are the strings the drain named.
func (oc *ownedCtx) owns(e *Event) bool {
	return sameString(&oc.event, &oc.eventLen, e.Event) && sameString(&oc.data, &oc.dataLen, e.Data)
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

// formatComment writes a comment line ": <text>\n\n" into buf. Newlines
// inside text are stripped to prevent SSE-event injection: a `\n` would
// terminate the comment line and let attacker-controlled trailing bytes
// be interpreted as `data:` / `event:` / `id:` fields.
func formatComment(buf []byte, text string) []byte {
	buf = buf[:0]
	buf = append(buf, ": "...)
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\r' || c == '\n' {
			continue
		}
		buf = append(buf, c)
	}
	buf = append(buf, '\n', '\n')
	return buf
}
