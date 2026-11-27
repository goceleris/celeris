package celeris

import "testing"

// Server.AsyncHandlers resolves an unset Config.Engine to the engine Start runs
// (Adaptive on Linux, Std elsewhere): a default-engine server that has async
// handlers reports true, one that has none reports false (celeris#1376).
func TestAsyncHandlersUnsetEngine1376(t *testing.T) {
	if New(Config{}).AsyncHandlers() {
		t.Error("default engine, no async handler: AsyncHandlers() = true, want false")
	}
	if !New(Config{AsyncHandlers: true}).AsyncHandlers() {
		t.Error("default engine, Config.AsyncHandlers: AsyncHandlers() = false, want true")
	}
	s := New(Config{})
	s.GET("/x", noopHandler).Async()
	if !s.AsyncHandlers() {
		t.Error("default engine, one .Async() route: AsyncHandlers() = false, want true")
	}
}
