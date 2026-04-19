package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// saveMap serializes data as JSON and writes it to kv under id. Helper
// used by session tests that previously called the old Store.Save.
func saveMap(tb testing.TB, kv store.KV, id string, data map[string]any, expiry time.Duration) {
	tb.Helper()
	buf, err := store.EncodeJSON(data)
	if err != nil {
		tb.Fatalf("encode session %q: %v", id, err)
	}
	if err := kv.Set(context.Background(), id, buf, expiry); err != nil {
		tb.Fatalf("save session %q: %v", id, err)
	}
}

// loadMap reads id from kv, decodes the JSON body into a map. Returns
// (nil, false) when the key is missing or expired.
func loadMap(tb testing.TB, kv store.KV, id string) (map[string]any, bool) {
	tb.Helper()
	raw, err := kv.Get(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false
	}
	if err != nil {
		tb.Fatalf("load session %q: %v", id, err)
	}
	var out map[string]any
	if err := store.DecodeJSON(raw, &out); err != nil {
		tb.Fatalf("decode session %q: %v", id, err)
	}
	return out, true
}

// asFloat coerces a JSON-decoded numeric value (which comes back as
// float64) or a Go-native int/int64 to float64 for test comparisons.
// JSON round-trip erases the int/int64/float64 distinction, so tests
// that assert numeric equality should compare via asFloat.
func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	}
	return 0
}
