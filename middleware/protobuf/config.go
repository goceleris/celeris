package protobuf

import (
	"errors"

	"google.golang.org/protobuf/proto"
)

// Content type constants for Protocol Buffers.
const (
	// ContentType is the primary MIME type for protobuf payloads.
	ContentType = "application/x-protobuf"
	// ContentTypeAlt is an alternative MIME type accepted for compatibility.
	ContentTypeAlt = "application/protobuf"
)

// Sentinel errors.
var (
	// ErrNilMessage is returned when a nil proto.Message is passed to ProtoBuf.
	ErrNilMessage = errors.New("protobuf: nil proto.Message")
	// ErrInvalidProtoBuf is returned when unmarshaling fails.
	ErrInvalidProtoBuf = errors.New("protobuf: invalid protobuf data")
	// ErrNotProtoBuf is returned by Bind when the content type is not protobuf.
	ErrNotProtoBuf = errors.New("protobuf: content type is not protobuf")
)

// wrapErr pairs ErrInvalidProtoBuf with the underlying proto unmarshal
// error. Replaces fmt.Errorf("%w: %w", ...) — same alloc-profile
// reason as middleware/jwt/classifiedError (R71) and
// middleware/jwt/internal/jwtparse/wrapErr (R72): fmt.Errorf with
// double-%w costs four allocations (printer state, message string,
// wrapErrors struct, unwrap slice); this is one struct alloc with
// a lazy Error() concat.
type wrapErr struct {
	outer error
	inner error
}

func (w *wrapErr) Error() string {
	return w.outer.Error() + ": " + w.inner.Error()
}

// Is matches target against either side, avoiding a per-call slice
// alloc that Unwrap() []error would require.
func (w *wrapErr) Is(target error) bool {
	if errors.Is(w.outer, target) {
		return true
	}
	return errors.Is(w.inner, target)
}

// Unwrap returns the outer sentinel for errors.As.
func (w *wrapErr) Unwrap() error { return w.outer }

// Config controls protobuf marshaling and unmarshaling options.
type Config struct {
	// MarshalOptions controls proto marshaling behavior.
	// Default: proto.MarshalOptions{} (non-deterministic for performance).
	MarshalOptions proto.MarshalOptions

	// UnmarshalOptions controls proto unmarshaling behavior.
	// Default: proto.UnmarshalOptions{DiscardUnknown: true} (safe default).
	UnmarshalOptions proto.UnmarshalOptions
}

var defaultConfig = Config{
	UnmarshalOptions: proto.UnmarshalOptions{DiscardUnknown: true},
}
