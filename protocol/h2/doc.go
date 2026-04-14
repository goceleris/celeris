// Package h2 provides HTTP/2 frame handling and stream management.
//
// Subpackage [protocol/h2/frame] implements the wire-level frame
// codec (HEADERS, DATA, SETTINGS, WINDOW_UPDATE, GOAWAY, RST_STREAM,
// PUSH_PROMISE, PING, CONTINUATION, PRIORITY) and HPACK
// compression/decompression. Subpackage [protocol/h2/stream] implements
// per-stream lifecycle (state machine, flow-control windows, request/
// response buffering, trailers).
//
// Application code should not import these packages directly — interact
// with HTTP/2 through the celeris [Server] and [Context] types.
package h2
