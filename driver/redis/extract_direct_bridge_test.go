package redis

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// TestDispatch_BridgeTypedRequest_TakesExtractDirect verifies the
// post-a23d19a behaviour: when a typed redisRequest (expect != expectNone)
// is dispatched via the bridge — the path the pipeline tail-spillover hits
// after WriteAndPollMulti returns ok=false and pollBeforeRearm migrates
// remaining requests off syncPipeReqs — the extractor populates the typed
// scalar field and leaves req.result at its zero value (no protocol.Value
// copy alloc).
func TestDispatch_BridgeTypedRequest_TakesExtractDirect(t *testing.T) {
	s := newRedisState()
	req := getRequest(context.Background())
	req.expect = expectInt
	t.Cleanup(func() { putRequest(req) })
	s.bridge.Enqueue(req)

	// Feed a single ":42\r\n" RESP integer; processRedis decodes via Reader
	// and routes through dispatch — bridge mode (syncPipeReqs is nil).
	if err := s.processRedis([]byte(":42\r\n")); err != nil {
		t.Fatalf("processRedis: %v", err)
	}

	if req.intResult != 42 {
		t.Fatalf("intResult = %d, want 42 (typed extraction did not run)", req.intResult)
	}
	if req.expect != expectInt {
		t.Fatalf("expect = %v, want expectInt (fallback flipped expect=expectNone)", req.expect)
	}
	if req.result.Int != 0 {
		t.Fatalf("result.Int = %d, want 0 (typed extraction must not also populate Value)", req.result.Int)
	}
	if !req.finished.Load() {
		t.Fatalf("finished should be true after dispatch")
	}
}

// TestDispatch_BridgeUntypedRequest_TakesValueCopy is the negative
// control: expectNone (single-cmd path / unknown-type pipeline cmd) keeps
// the legacy copyValueDetached fallback.
func TestDispatch_BridgeUntypedRequest_TakesValueCopy(t *testing.T) {
	s := newRedisState()
	req := getRequest(context.Background())
	// expect is expectNone by default — explicitly state for clarity.
	req.expect = expectNone
	t.Cleanup(func() { putRequest(req) })
	s.bridge.Enqueue(req)

	if err := s.processRedis([]byte(":42\r\n")); err != nil {
		t.Fatalf("processRedis: %v", err)
	}
	if req.result.Type != protocol.TyInt || req.result.Int != 42 {
		t.Fatalf("result = {%v, %d}, want TyInt=42 (untyped path must populate Value)", req.result.Type, req.result.Int)
	}
	if req.intResult != 0 {
		t.Fatalf("intResult = %d, want 0 (untyped path must not touch typed fields)", req.intResult)
	}
}
