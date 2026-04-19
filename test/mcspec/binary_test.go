//go:build memcached

package mcspec

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// TestBinary_HeaderRoundTrip validates the fixed 24-byte header framing by
// sending a Noop (no extras, no key, no value) and confirming we get exactly
// a 24-byte response with magic 0x81, opcode 0x0a, status 0x0000.
func TestBinary_HeaderRoundTrip(t *testing.T) {
	c := dialBinary(t)
	opaque := c.nextOpaque()
	c.writer.Reset()
	c.writer.AppendSimple(protocol.OpNoop, opaque)
	c.WriteRaw(c.writer.Bytes())

	pkt := c.ExpectStatus(t, protocol.OpNoop, protocol.StatusOK)
	if pkt.Header.Opaque != opaque {
		t.Fatalf("opaque = 0x%08x, want 0x%08x", pkt.Header.Opaque, opaque)
	}
	if pkt.Header.BodyLen != 0 {
		t.Fatalf("Noop BodyLen = %d, want 0", pkt.Header.BodyLen)
	}
	if pkt.Header.KeyLen != 0 || pkt.Header.ExtrasLen != 0 {
		t.Fatalf("Noop KeyLen/ExtrasLen not zero: %+v", pkt.Header)
	}
}

// TestBinary_Set_Get_Delete exercises the core storage+retrieval cycle over
// binary.
func TestBinary_Set_Get_Delete(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_sgd")
	val := []byte("binaryvalue")

	// SET
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, val, 0xdeadbeef, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	setPkt := c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)
	setCAS := setPkt.Header.CAS
	if setCAS == 0 {
		t.Fatalf("SET reply CAS is zero")
	}

	// GET
	c.writer.Reset()
	c.writer.AppendGet(protocol.OpGet, k, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	getPkt := c.ExpectStatus(t, protocol.OpGet, protocol.StatusOK)
	// GET reply has 4-byte flags extras + no key + value body.
	if len(getPkt.Extras) != 4 {
		t.Fatalf("GET extras len = %d, want 4", len(getPkt.Extras))
	}
	flags := binary.BigEndian.Uint32(getPkt.Extras)
	if flags != 0xdeadbeef {
		t.Fatalf("GET flags = 0x%08x, want 0xdeadbeef", flags)
	}
	if !bytes.Equal(getPkt.Value, val) {
		t.Fatalf("GET value = %q, want %q", getPkt.Value, val)
	}

	// DELETE
	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpDelete, protocol.StatusOK)

	// GET after delete -> KEY_NOT_FOUND
	c.writer.Reset()
	c.writer.AppendGet(protocol.OpGet, k, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpGet, protocol.StatusKeyNotFound)
}

// TestBinary_AddReplace exercises Add/Replace and their NOT_STORED /
// KEY_EXISTS semantics.
func TestBinary_AddReplace(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_ar")

	// Add on missing -> OK
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpAdd, k, []byte("first"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpAdd, protocol.StatusOK)

	// Add on existing -> KEY_EXISTS (or ITEM_NOT_STORED depending on server)
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpAdd, k, []byte("second"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ReadPacket(t)
	if pkt.Status() != protocol.StatusKeyExists && pkt.Status() != protocol.StatusItemNotStored {
		t.Fatalf("Add dup status = 0x%04x, want KEY_EXISTS or ITEM_NOT_STORED", pkt.Status())
	}

	// Replace on existing -> OK
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpReplace, k, []byte("replaced"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpReplace, protocol.StatusOK)

	// Replace on missing -> KEY_NOT_FOUND (or ITEM_NOT_STORED on some builds)
	missing := uniqueKey(t, "bin_ar_miss")
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpReplace, missing, []byte("v"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt = c.ReadPacket(t)
	if pkt.Status() != protocol.StatusKeyNotFound && pkt.Status() != protocol.StatusItemNotStored {
		t.Fatalf("Replace missing status = 0x%04x, want KEY_NOT_FOUND or ITEM_NOT_STORED", pkt.Status())
	}

	// cleanup
	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

// TestBinary_CAS verifies that a binary SET with a stale CAS returns KEY_EXISTS.
func TestBinary_CAS(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_cas")

	// Initial SET, record CAS.
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, []byte("v0"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)
	cas := pkt.Header.CAS

	// SET with stale CAS -> KEY_EXISTS
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, []byte("v1"), 0, 0, cas+1, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpSet, protocol.StatusKeyExists)

	// SET with correct CAS -> OK
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, []byte("v1"), 0, 0, cas, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)

	// cleanup
	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

// TestBinary_IncrDecr verifies the arithmetic opcodes. The reply body is a
// big-endian uint64 representing the new counter value.
func TestBinary_IncrDecr(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_arith")

	// Incr with initial=10, delta=0, exptime=0 creates the counter at 10.
	c.writer.Reset()
	c.writer.AppendArith(protocol.OpIncrement, k, 0, 10, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ExpectStatus(t, protocol.OpIncrement, protocol.StatusOK)
	if len(pkt.Value) != 8 {
		t.Fatalf("incr body len = %d, want 8", len(pkt.Value))
	}
	if binary.BigEndian.Uint64(pkt.Value) != 10 {
		t.Fatalf("incr value = %d, want 10", binary.BigEndian.Uint64(pkt.Value))
	}

	// Incr with delta=5 -> 15
	c.writer.Reset()
	c.writer.AppendArith(protocol.OpIncrement, k, 5, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt = c.ExpectStatus(t, protocol.OpIncrement, protocol.StatusOK)
	if binary.BigEndian.Uint64(pkt.Value) != 15 {
		t.Fatalf("incr value = %d, want 15", binary.BigEndian.Uint64(pkt.Value))
	}

	// Decr with delta=3 -> 12
	c.writer.Reset()
	c.writer.AppendArith(protocol.OpDecrement, k, 3, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt = c.ExpectStatus(t, protocol.OpDecrement, protocol.StatusOK)
	if binary.BigEndian.Uint64(pkt.Value) != 12 {
		t.Fatalf("decr value = %d, want 12", binary.BigEndian.Uint64(pkt.Value))
	}

	// Decr missing with exptime=0xFFFFFFFF -> KEY_NOT_FOUND
	missing := uniqueKey(t, "bin_arith_missing")
	c.writer.Reset()
	c.writer.AppendArith(protocol.OpDecrement, missing, 1, 0, 0xFFFFFFFF, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpDecrement, protocol.StatusKeyNotFound)

	// cleanup
	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

// TestBinary_AppendPrepend verifies the concat opcodes (no extras in the
// request packet).
func TestBinary_AppendPrepend(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_concat")

	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, []byte("mid"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)

	c.writer.Reset()
	c.writer.AppendConcat(protocol.OpAppend, k, []byte("_tail"), 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpAppend, protocol.StatusOK)

	c.writer.Reset()
	c.writer.AppendConcat(protocol.OpPrepend, k, []byte("head_"), 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpPrepend, protocol.StatusOK)

	c.writer.Reset()
	c.writer.AppendGet(protocol.OpGet, k, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ExpectStatus(t, protocol.OpGet, protocol.StatusOK)
	if !bytes.Equal(pkt.Value, []byte("head_mid_tail")) {
		t.Fatalf("concat value = %q, want head_mid_tail", pkt.Value)
	}

	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

// TestBinary_Touch exercises OpTouch and its miss semantics.
func TestBinary_Touch(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_touch")

	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, []byte("v"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)

	c.writer.Reset()
	c.writer.AppendTouch(k, 60, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpTouch, protocol.StatusOK)

	// Miss
	missing := uniqueKey(t, "bin_touch_missing")
	c.writer.Reset()
	c.writer.AppendTouch(missing, 60, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpTouch, protocol.StatusKeyNotFound)

	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

// TestBinary_Version verifies OpVersion round-trips a non-empty value body.
func TestBinary_Version(t *testing.T) {
	c := dialBinary(t)
	c.writer.Reset()
	c.writer.AppendSimple(protocol.OpVersion, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ExpectStatus(t, protocol.OpVersion, protocol.StatusOK)
	if len(pkt.Value) == 0 {
		t.Fatal("OpVersion reply body is empty")
	}
}

// TestBinary_UnknownCommand verifies OpUnknown (0xFE or 0xFF, picked to be
// outside the implemented set) returns StatusUnknownCommand.
func TestBinary_UnknownCommand(t *testing.T) {
	c := dialBinary(t)
	c.writer.Reset()
	// Opcode 0xFF is not defined in the spec; all memcached builds respond
	// with StatusUnknownCommand.
	c.writer.AppendSimple(0xFF, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ReadPacket(t)
	if pkt.Status() != protocol.StatusUnknownCommand {
		t.Fatalf("unknown opcode status = 0x%04x, want 0x%04x", pkt.Status(), protocol.StatusUnknownCommand)
	}
}

// TestBinarySpec_GetMultiPipelinedFraming writes an OpGetKQ pipeline + OpNoop
// terminator directly on the raw socket and asserts the server emits the
// expected per-key response packets followed by the Noop echo. Validates the
// driver's on-wire framing contract at the protocol layer: misses suppress
// their reply entirely, hits carry the key back in the response body, and
// the Noop reply closes the stream.
func TestBinarySpec_GetMultiPipelinedFraming(t *testing.T) {
	c := dialBinary(t)

	// Seed one present key so the spec covers both hit and miss suppression.
	presentKey := uniqueKey(t, "sp_gmp_hit")
	absentA := uniqueKey(t, "sp_gmp_missA")
	absentB := uniqueKey(t, "sp_gmp_missB")

	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, presentKey, []byte("VAL"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)

	// Emit OpGetKQ x3 + OpNoop in a single write buffer.
	c.writer.Reset()
	opA := c.nextOpaque()
	opB := c.nextOpaque()
	opC := c.nextOpaque()
	opN := c.nextOpaque()
	c.writer.AppendGet(protocol.OpGetKQ, absentA, opA)
	c.writer.AppendGet(protocol.OpGetKQ, presentKey, opB)
	c.writer.AppendGet(protocol.OpGetKQ, absentB, opC)
	c.writer.AppendSimple(protocol.OpNoop, opN)
	// Sanity-check the buffer we're about to send contains the expected
	// sequence of request opcodes. This is the "driver's on-the-wire byte
	// output" part of the spec: exactly 3 OpGetKQ + 1 OpNoop in order.
	wire := c.writer.Bytes()
	var seenOpcodes []byte
	i := 0
	for i+protocol.BinHeaderLen <= len(wire) {
		if wire[i] != protocol.MagicRequest {
			t.Fatalf("bad magic in request buffer at offset %d: 0x%02x", i, wire[i])
		}
		seenOpcodes = append(seenOpcodes, wire[i+1])
		bodyLen := int(wire[i+8])<<24 | int(wire[i+9])<<16 | int(wire[i+10])<<8 | int(wire[i+11])
		i += protocol.BinHeaderLen + bodyLen
	}
	want := []byte{protocol.OpGetKQ, protocol.OpGetKQ, protocol.OpGetKQ, protocol.OpNoop}
	if len(seenOpcodes) != len(want) {
		t.Fatalf("request buffer opcodes = %x, want %x", seenOpcodes, want)
	}
	for j := range want {
		if seenOpcodes[j] != want[j] {
			t.Fatalf("request opcode[%d] = 0x%02x, want 0x%02x", j, seenOpcodes[j], want[j])
		}
	}
	c.WriteRaw(wire)

	// Expect exactly one GetKQ hit (the present key) and one Noop, in any
	// order that misses suppressed the other two replies. Per spec, server
	// responds to OpGetKQ hits with opcode 0x0d and to OpNoop with 0x0a.
	first := c.ReadPacket(t)
	second := c.ReadPacket(t)
	// Either order is legal (miss suppression means hits and noop order is
	// server-defined); but memcached always emits replies in request order
	// so the hit should precede the Noop. We tolerate both for robustness.
	var hit, noop protocol.BinaryPacket
	switch {
	case first.Header.Opcode == protocol.OpGetKQ && second.Header.Opcode == protocol.OpNoop:
		hit, noop = first, second
	case first.Header.Opcode == protocol.OpNoop && second.Header.Opcode == protocol.OpGetKQ:
		hit, noop = second, first
	default:
		t.Fatalf("unexpected reply opcodes: 0x%02x + 0x%02x (want 0x0d + 0x0a)",
			first.Header.Opcode, second.Header.Opcode)
	}
	if hit.Status() != protocol.StatusOK {
		t.Fatalf("hit status = 0x%04x, want OK", hit.Status())
	}
	if string(hit.Key) != presentKey {
		t.Fatalf("hit key = %q, want %q", hit.Key, presentKey)
	}
	if string(hit.Value) != "VAL" {
		t.Fatalf("hit value = %q, want %q", hit.Value, "VAL")
	}
	if hit.Header.Opaque != opB {
		t.Fatalf("hit opaque = 0x%08x, want 0x%08x", hit.Header.Opaque, opB)
	}
	if noop.Status() != protocol.StatusOK {
		t.Fatalf("noop status = 0x%04x, want OK", noop.Status())
	}
	if noop.Header.BodyLen != 0 {
		t.Fatalf("noop BodyLen = %d, want 0", noop.Header.BodyLen)
	}
	if noop.Header.Opaque != opN {
		t.Fatalf("noop opaque = 0x%08x, want 0x%08x", noop.Header.Opaque, opN)
	}

	// cleanup
	c.writer.Reset()
	c.writer.AppendDelete(presentKey, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

// TestBinarySpec_StatsTerminatorFraming sends an OpStat request and reads
// packets until the zero-key + zero-body terminator arrives, then confirms
// the server responds normally to a follow-up op (no stream-state leak).
func TestBinarySpec_StatsTerminatorFraming(t *testing.T) {
	c := dialBinary(t)
	c.writer.Reset()
	c.writer.AppendStats("", c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())

	count := 0
	for {
		pkt := c.ReadPacket(t)
		if pkt.Header.Opcode != protocol.OpStat {
			t.Fatalf("unexpected opcode in stats stream: 0x%02x", pkt.Header.Opcode)
		}
		if pkt.Status() != protocol.StatusOK {
			t.Fatalf("stats packet status = 0x%04x, want OK", pkt.Status())
		}
		if pkt.Header.KeyLen == 0 && pkt.Header.BodyLen == 0 {
			break
		}
		count++
		if count > 10000 {
			t.Fatal("stats stream did not terminate after 10000 packets")
		}
	}
	if count == 0 {
		t.Fatal("stats stream produced no non-terminator packets")
	}

	// Follow-up version request; confirms the stream really terminated and
	// did not leak bytes into the next read.
	c.writer.Reset()
	c.writer.AppendSimple(protocol.OpVersion, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	pkt := c.ExpectStatus(t, protocol.OpVersion, protocol.StatusOK)
	if len(pkt.Value) == 0 {
		t.Fatal("version body empty after stats")
	}
}

// TestBinary_DeltaBadval verifies that incr on a non-numeric value returns
// StatusNonNumeric (DELTA_BADVAL-equivalent).
func TestBinary_DeltaBadval(t *testing.T) {
	c := dialBinary(t)
	k := uniqueKey(t, "bin_badval")

	// Store a non-numeric value.
	c.writer.Reset()
	c.writer.AppendStorage(protocol.OpSet, k, []byte("notanumber"), 0, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpSet, protocol.StatusOK)

	// Incr on non-numeric -> StatusNonNumeric
	c.writer.Reset()
	c.writer.AppendArith(protocol.OpIncrement, k, 1, 0, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	c.ExpectStatus(t, protocol.OpIncrement, protocol.StatusNonNumeric)

	c.writer.Reset()
	c.writer.AppendDelete(k, 0, c.nextOpaque())
	c.WriteRaw(c.writer.Bytes())
	_ = c.ReadPacket(t)
}

