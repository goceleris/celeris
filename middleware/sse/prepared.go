package sse

// PreparedEvent caches the wire bytes of a single SSE event so the same
// event can be broadcast to N subscribers with one [FormatEvent] call.
// It mirrors the role of websocket.PreparedMessage in the broadcast
// fan-out path.
//
// Once constructed a PreparedEvent is immutable and safe for concurrent
// reads from any number of [Client.WritePreparedEvent] callers.
type PreparedEvent struct {
	bytes []byte
}

// NewPreparedEvent formats e once into the SSE wire format and returns a
// PreparedEvent backed by the formatted bytes.
func NewPreparedEvent(e Event) *PreparedEvent {
	return &PreparedEvent{bytes: formatEvent(nil, &e)}
}

// Len reports the byte length of the formatted wire payload — useful
// for accounting + flow-control without exposing the cached bytes (and
// matching the asymmetric API of websocket.PreparedMessage, which
// also keeps its frame bytes private).
func (pe *PreparedEvent) Len() int { return len(pe.bytes) }

// WritePreparedEvent writes a [PreparedEvent] directly to the underlying
// stream, skipping the per-call [FormatEvent] step that [Client.Send]
// performs. Useful when the same event is fanned out to many clients.
//
// Thread-safe; serialises with concurrent [Client.Send] and the heartbeat
// writer through c.mu. Returns [ErrClientClosed] after the client has been
// closed and the underlying context error after disconnect.
//
// Always synchronous (writes to the wire under the lock) — bypasses
// [Config.MaxQueueDepth]. The [Broker] is the layer above this method:
// it provides its own per-subscriber queue (sized by
// [BrokerConfig.SubscriberBuffer]) and a drain goroutine per subscriber
// that calls WritePreparedEvent — adding another layer of buffering at
// this level would be redundant.
func (c *Client) WritePreparedEvent(pe *PreparedEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClientClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}

	if _, err := c.sw.Write(pe.bytes); err != nil {
		c.cancel()
		return err
	}
	if err := c.sw.Flush(); err != nil {
		c.cancel()
		return err
	}
	return nil
}
