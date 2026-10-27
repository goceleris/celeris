package conn

import "sync/atomic"

// BacklogRefusedHook is a test seam (celeris#890 item 22). It is nil in
// production. When a request finds the connection's unsent responses over the
// engine's back-pressure limit (H1State.WriteBacklogged) and is therefore not
// served (ErrWriteBacklog), a test that installed it is told which connection
// by its peer address (H1State.RemoteAddr). That is the server-side event a
// test of the limit has to wait for, instead of a fixed sleep after the
// kernel's send queue has stopped growing: once it has fired the engine has
// decided to close the connection when what is queued has gone out, whatever
// the client does next.
//
// It is read only on the refusal, never on the path of a request that is
// served, so it costs a request nothing.
var BacklogRefusedHook atomic.Pointer[func(remoteAddr string)]
