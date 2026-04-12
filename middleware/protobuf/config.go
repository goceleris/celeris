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
