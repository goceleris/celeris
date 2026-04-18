//go:build memcached

package mcspec

import (
	"bytes"
	crand "crypto/rand"
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// ---------------------------------------------------------------------------
// Storage replies: STORED / NOT_STORED / EXISTS / NOT_FOUND / DELETED /
// TOUCHED / OK / END
// ---------------------------------------------------------------------------

// TestText_Stored verifies that a valid set command produces "STORED\r\n".
func TestText_Stored(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "stored")

	c.WriteStorage("set", k, 0, 0, []byte("hello"), 0)
	c.ExpectKind(t, protocol.KindStored)

	// cleanup
	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// TestText_NotStored covers "NOT_STORED\r\n" via Add on an existing key and
// Replace on a missing one.
func TestText_NotStored(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "notstored")

	c.WriteStorage("set", k, 0, 0, []byte("v"), 0)
	c.ExpectKind(t, protocol.KindStored)

	// Add on existing key -> NOT_STORED
	c.WriteStorage("add", k, 0, 0, []byte("v2"), 0)
	c.ExpectKind(t, protocol.KindNotStored)

	// Replace on missing key -> NOT_STORED
	missing := uniqueKey(t, "notstored_missing")
	c.WriteStorage("replace", missing, 0, 0, []byte("v"), 0)
	c.ExpectKind(t, protocol.KindNotStored)

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// TestText_ExistsAndNotFound covers "EXISTS\r\n" (CAS mismatch) and
// "NOT_FOUND\r\n" (delete on missing key).
func TestText_ExistsAndNotFound(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "exists")
	c.WriteStorage("set", k, 0, 0, []byte("v0"), 0)
	c.ExpectKind(t, protocol.KindStored)

	// gets to learn the CAS.
	c.WriteLine("gets " + k)
	v := c.ExpectKind(t, protocol.KindValue)
	if v.CAS == 0 {
		t.Fatal("gets returned zero CAS")
	}
	c.ExpectKind(t, protocol.KindEnd)

	// CAS with a stale token -> EXISTS
	c.WriteStorage("cas", k, 0, 0, []byte("v1"), v.CAS+9999)
	c.ExpectKind(t, protocol.KindExists)

	// CAS with fresh token -> STORED
	c.WriteStorage("cas", k, 0, 0, []byte("v1"), v.CAS)
	c.ExpectKind(t, protocol.KindStored)

	// Delete missing key -> NOT_FOUND
	missing := uniqueKey(t, "notfound")
	c.WriteLine("delete " + missing)
	c.ExpectKind(t, protocol.KindNotFound)

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// TestText_TouchedAndOK covers "TOUCHED\r\n" (touch on a live key) and
// "OK\r\n" (cache_memlimit — a non-destructive command that emits OK, unlike
// flush_all which would invalidate adjacent tests).
func TestText_TouchedAndOK(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "touched")
	c.WriteStorage("set", k, 0, 0, []byte("v"), 0)
	c.ExpectKind(t, protocol.KindStored)

	c.WriteLine(fmt.Sprintf("touch %s 60", k))
	c.ExpectKind(t, protocol.KindTouched)

	// touch on missing -> NOT_FOUND
	c.WriteLine("touch " + uniqueKey(t, "touchmiss") + " 60")
	c.ExpectKind(t, protocol.KindNotFound)

	// cache_memlimit returns OK without stamping any items. Using flush_all
	// here would invalidate keys set by subsequent tests (memcached 1.6
	// applies the exptime to the "current" item set at command time, and
	// some builds extend the window briefly into the future).
	c.WriteLine("cache_memlimit 128")
	c.ExpectKind(t, protocol.KindOK)
}

// ---------------------------------------------------------------------------
// Retrieval reply framing
// ---------------------------------------------------------------------------

// TestText_SingleGetFraming validates the VALUE ... END framing for a single
// get.
func TestText_SingleGetFraming(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "framing")
	val := []byte("abcdef")

	c.WriteStorage("set", k, 42, 0, val, 0)
	c.ExpectKind(t, protocol.KindStored)

	c.WriteLine("get " + k)
	v := c.ExpectKind(t, protocol.KindValue)
	if string(v.Key) != k {
		t.Fatalf("Key = %q, want %q", v.Key, k)
	}
	if v.Flags != 42 {
		t.Fatalf("Flags = %d, want 42", v.Flags)
	}
	if !bytes.Equal(v.Data, val) {
		t.Fatalf("Data = %q, want %q", v.Data, val)
	}
	c.ExpectKind(t, protocol.KindEnd)

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// TestText_MultiGetFraming validates framing for `get k1 k2 k3` with a mix
// of present and absent keys — server returns VALUE blocks only for hits,
// followed by a single END.
func TestText_MultiGetFraming(t *testing.T) {
	c := dialText(t)
	k1 := uniqueKey(t, "mg_a")
	k2 := uniqueKey(t, "mg_b")
	k3 := uniqueKey(t, "mg_c")

	c.WriteStorage("set", k1, 0, 0, []byte("aaa"), 0)
	c.ExpectKind(t, protocol.KindStored)
	c.WriteStorage("set", k3, 0, 0, []byte("ccc"), 0)
	c.ExpectKind(t, protocol.KindStored)

	c.WriteLine(fmt.Sprintf("get %s %s %s", k1, k2, k3))

	got := map[string]string{}
	for {
		r := c.ReadReply(t)
		if r.Kind == protocol.KindEnd {
			break
		}
		if r.Kind != protocol.KindValue {
			t.Fatalf("unexpected kind %s before END", r.Kind)
		}
		got[string(r.Key)] = string(r.Data)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 hits, got %d: %v", len(got), got)
	}
	if got[k1] != "aaa" || got[k3] != "ccc" {
		t.Fatalf("multi-get values: %v", got)
	}
	if _, ok := got[k2]; ok {
		t.Fatalf("missing key %s unexpectedly present as %q", k2, got[k2])
	}

	c.WriteLine("delete " + k1)
	c.ExpectKind(t, protocol.KindDeleted)
	c.WriteLine("delete " + k3)
	c.ExpectKind(t, protocol.KindDeleted)
}

// TestText_CASInGetsReply verifies that `gets` includes a CAS token.
func TestText_CASInGetsReply(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "getscas")
	c.WriteStorage("set", k, 7, 0, []byte("x"), 0)
	c.ExpectKind(t, protocol.KindStored)

	c.WriteLine("gets " + k)
	v := c.ExpectKind(t, protocol.KindValue)
	if v.CAS == 0 {
		t.Fatalf("gets returned zero CAS")
	}
	if v.Flags != 7 {
		t.Fatalf("Flags = %d, want 7", v.Flags)
	}
	c.ExpectKind(t, protocol.KindEnd)

	// CAS round-trip: update succeeds, repeat with old CAS fails.
	c.WriteStorage("cas", k, 7, 0, []byte("y"), v.CAS)
	c.ExpectKind(t, protocol.KindStored)
	c.WriteStorage("cas", k, 7, 0, []byte("z"), v.CAS)
	c.ExpectKind(t, protocol.KindExists)

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// ---------------------------------------------------------------------------
// Error reply types
// ---------------------------------------------------------------------------

// TestText_ErrorReply exercises the "ERROR\r\n" reply, emitted when the server
// encounters a wholly unknown command.
func TestText_ErrorReply(t *testing.T) {
	c := dialText(t)
	c.WriteLine("this_is_not_a_real_memcached_command")
	c.ExpectKind(t, protocol.KindError)
}

// TestText_ClientErrorReply exercises "CLIENT_ERROR <msg>\r\n". A malformed
// "set" (non-numeric flags) triggers it reliably.
func TestText_ClientErrorReply(t *testing.T) {
	c := dialText(t)
	// Manually frame a bad set: flags = "abc".
	c.WriteRaw([]byte("set k abc 0 1\r\nx\r\n"))
	r := c.ReadReply(t)
	if r.Kind != protocol.KindClientError {
		t.Fatalf("expected CLIENT_ERROR, got %s (data=%q)", r.Kind, r.Data)
	}
	if len(r.Data) == 0 {
		t.Fatal("CLIENT_ERROR message is empty")
	}
}

// TestText_ServerErrorReply is tricky to trigger in a portable way. We skip
// asserting it (the path is exercised by the driver's unit tests), but leave
// a placeholder so the lineage is obvious.
func TestText_ServerErrorReply(t *testing.T) {
	t.Skip("SERVER_ERROR requires out-of-memory or admin-disabled conditions that aren't portable")
}

// ---------------------------------------------------------------------------
// Binary-safe payload
// ---------------------------------------------------------------------------

// TestText_BinarySafeValue stores a 256-byte random payload (including zeros
// and high bytes) and verifies the data block is byte-identical on read.
func TestText_BinarySafeValue(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "binvalue")
	payload := make([]byte, 256)
	if _, err := crand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Force a few key boundary bytes just in case.
	payload[0] = 0x00
	payload[1] = '\r'
	payload[2] = '\n'
	payload[255] = 0xff

	c.WriteStorage("set", k, 0, 0, payload, 0)
	c.ExpectKind(t, protocol.KindStored)

	c.WriteLine("get " + k)
	v := c.ExpectKind(t, protocol.KindValue)
	if !bytes.Equal(v.Data, payload) {
		t.Fatalf("binary-safe round trip failed: got %d bytes", len(v.Data))
	}
	c.ExpectKind(t, protocol.KindEnd)

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// ---------------------------------------------------------------------------
// Large-value framing at the default -I cap (~1 MiB)
// ---------------------------------------------------------------------------

// TestText_LargeValueAtIOCap stores a 1,000,000-byte (just under the 1 MiB
// default cap) blob and verifies the round-trip. memcached's -I default is
// 1 MiB and includes per-item overhead, so we stay a few bytes below.
func TestText_LargeValueAtIOCap(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "large")

	size := 1 * 1000 * 1000 // 1,000,000 — comfortably below 1 MiB minus item header
	payload := make([]byte, size)
	if _, err := crand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	c.WriteStorage("set", k, 0, 0, payload, 0)
	r := c.ReadReply(t)
	if r.Kind == protocol.KindServerError {
		t.Skipf("server rejected 1MB payload (raise -I); SERVER_ERROR %q", r.Data)
	}
	if r.Kind != protocol.KindStored {
		t.Fatalf("expected STORED, got %s (data=%q)", r.Kind, r.Data)
	}

	c.WriteLine("get " + k)
	v := c.ExpectKind(t, protocol.KindValue)
	if len(v.Data) != size {
		t.Fatalf("got %d bytes, want %d", len(v.Data), size)
	}
	if !bytes.Equal(v.Data, payload) {
		// Find the first differing byte for a meaningful diag.
		var diff int
		for diff = 0; diff < size; diff++ {
			if v.Data[diff] != payload[diff] {
				break
			}
		}
		t.Fatalf("large payload mismatch at byte %d", diff)
	}
	c.ExpectKind(t, protocol.KindEnd)

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}

// TestText_VersionReply verifies the VERSION reply is recognized and carries
// a non-empty value.
func TestText_VersionReply(t *testing.T) {
	c := dialText(t)
	c.WriteLine("version")
	v := c.ExpectKind(t, protocol.KindVersion)
	if len(strings.TrimSpace(string(v.Data))) == 0 {
		t.Fatal("VERSION payload is empty")
	}
}

// TestText_IncrNumberReply verifies that incr returns a bare decimal integer
// reply.
func TestText_IncrNumberReply(t *testing.T) {
	c := dialText(t)
	k := uniqueKey(t, "num")

	c.WriteStorage("set", k, 0, 0, []byte("10"), 0)
	c.ExpectKind(t, protocol.KindStored)

	c.WriteLine("incr " + k + " 5")
	r := c.ExpectKind(t, protocol.KindNumber)
	if r.Int != 15 {
		t.Fatalf("incr = %d, want 15", r.Int)
	}

	c.WriteLine("delete " + k)
	c.ExpectKind(t, protocol.KindDeleted)
}
