package protobuf

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/goceleris/celeris"
)

// maxPooledBufSize is the maximum buffer capacity returned to the pool.
// Buffers that grow beyond this threshold (e.g., when marshaling large
// messages) are discarded to avoid holding oversized memory in the pool
// for subsequent small messages. Wire PoolEvictions into your metrics
// pipeline to detect if your message sizes consistently exceed this limit.
const maxPooledBufSize = 32 * 1024 // 32KB

// PoolEvictions counts how many times a marshal buffer exceeded
// maxPooledBufSize and was discarded instead of returned to the pool.
// Monotonically increasing; safe for concurrent reads.
var PoolEvictions atomic.Uint64

var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 256)
	return &b
}}

// Write serializes v as Protocol Buffers and writes it with the given
// status code and content type "application/x-protobuf".
func Write(c *celeris.Context, code int, v proto.Message) error {
	return protoBufWith(c, code, v, proto.MarshalOptions{})
}

func protoBufWith(c *celeris.Context, code int, v proto.Message, opts proto.MarshalOptions) error {
	if v == nil {
		return ErrNilMessage
	}
	bp := bufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	data, err := opts.MarshalAppend(buf, v)
	if err != nil {
		*bp = buf
		bufPool.Put(bp)
		return err
	}
	writeErr := c.Blob(code, ContentType, data)
	// Return buffer to pool (Blob copies data internally).
	if cap(data) <= maxPooledBufSize {
		*bp = data
		bufPool.Put(bp)
	} else {
		PoolEvictions.Add(1)
	}
	return writeErr
}

// BindProtoBuf reads the request body and unmarshals it into v as Protocol Buffers.
// Returns [celeris.ErrEmptyBody] if the body is empty.
func BindProtoBuf(c *celeris.Context, v proto.Message) error {
	return bindProtoBufWith(c, v, proto.UnmarshalOptions{DiscardUnknown: true})
}

func bindProtoBufWith(c *celeris.Context, v proto.Message, opts proto.UnmarshalOptions) error {
	body := c.Body()
	if len(body) == 0 {
		return celeris.ErrEmptyBody
	}
	if err := opts.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProtoBuf, err)
	}
	return nil
}

// Bind reads the request body and unmarshals it into v, but only if the
// Content-Type header indicates protobuf (application/x-protobuf or
// application/protobuf). Returns [ErrNotProtoBuf] if the content type
// does not match.
func Bind(c *celeris.Context, v proto.Message) error {
	ct := c.Header("content-type")
	if !isProtoBufContentType(ct) {
		return ErrNotProtoBuf
	}
	return BindProtoBuf(c, v)
}

// Respond performs content negotiation between protobuf and JSON. If the
// client accepts protobuf (via Accept header), v is serialized as protobuf.
// Otherwise, jsonFallback is serialized as JSON.
func Respond(c *celeris.Context, code int, v proto.Message, jsonFallback any) error {
	best := c.Negotiate(ContentType, ContentTypeAlt, "application/json")
	if best == ContentType || best == ContentTypeAlt {
		return Write(c, code, v)
	}
	return c.JSON(code, jsonFallback)
}

func isProtoBufContentType(ct string) bool {
	ct = strings.TrimSpace(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	return strings.EqualFold(ct, ContentType) || strings.EqualFold(ct, ContentTypeAlt)
}
