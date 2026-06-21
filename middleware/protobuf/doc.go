// Package protobuf provides Protocol Buffers serialization for celeris.
//
// Protocol Buffers support was split from the celeris core to avoid pulling
// google.golang.org/protobuf as a transitive dependency. This package provides
// equivalent functionality as standalone helpers and an optional middleware.
//
// Key exports:
//   - [Write] — marshal a proto.Message and write it with status code.
//   - [BindProtoBuf] — read the request body and unmarshal it as protobuf.
//   - [Bind] — like BindProtoBuf but first checks the Content-Type header,
//     returning [ErrNotProtoBuf] when it is not a protobuf content type.
//   - [Respond] — content-negotiate between protobuf and JSON using the
//     Accept header, honoring q=0 exclusions per RFC 7231 §5.3.1.
//   - [New] / [Config] — optional middleware that stashes marshal/unmarshal
//     options in the request context for retrieval via [FromContext].
//   - [PoolEvictions] — monotonic counter for buffers discarded from the
//     internal marshal pool; wire into metrics to detect large messages.
//
// Both "application/x-protobuf" ([ContentType]) and "application/protobuf"
// ([ContentTypeAlt]) are accepted on inbound requests. Responses written by
// [Write] always use "application/x-protobuf".
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-content
package protobuf
