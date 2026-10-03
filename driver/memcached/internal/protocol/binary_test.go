package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestBinaryReaderRoundTrip(t *testing.T) {
	// Build a response packet by hand: OK status, key "foo", value "bar",
	// flags = 7 in the 4-byte extras.
	var extras [4]byte
	binary.BigEndian.PutUint32(extras[0:4], 7)
	key := []byte("foo")
	value := []byte("bar")
	var hdr [BinHeaderLen]byte
	hdr[0] = MagicResponse
	hdr[1] = OpGet
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(key)))
	hdr[4] = byte(len(extras))
	binary.BigEndian.PutUint16(hdr[6:8], StatusOK)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(extras)+len(key)+len(value)))
	binary.BigEndian.PutUint32(hdr[12:16], 42)  // opaque
	binary.BigEndian.PutUint64(hdr[16:24], 999) // CAS
	pkt := append([]byte{}, hdr[:]...)
	pkt = append(pkt, extras[:]...)
	pkt = append(pkt, key...)
	pkt = append(pkt, value...)

	r := NewBinaryReader()
	r.Feed(pkt)
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Magic != MagicResponse || got.Header.Opcode != OpGet {
		t.Fatalf("bad header: %#v", got.Header)
	}
	if got.Header.Opaque != 42 || got.Header.CAS != 999 {
		t.Fatalf("bad ids: %#v", got.Header)
	}
	if string(got.Key) != "foo" || string(got.Value) != "bar" {
		t.Fatalf("bad body: key=%q value=%q", got.Key, got.Value)
	}
	if len(got.Extras) != 4 || binary.BigEndian.Uint32(got.Extras) != 7 {
		t.Fatalf("bad extras: %v", got.Extras)
	}
}

func TestBinaryReaderIncomplete(t *testing.T) {
	r := NewBinaryReader()
	r.Feed([]byte{0x81, 0x00}) // partial header
	_, err := r.Next()
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
}

func TestBinaryReaderIncompleteBody(t *testing.T) {
	// Header advertises 10 bytes body but we only send 3 — must yield
	// ErrIncomplete and rewind.
	var hdr [BinHeaderLen]byte
	hdr[0] = MagicResponse
	binary.BigEndian.PutUint32(hdr[8:12], 10)
	r := NewBinaryReader()
	r.Feed(hdr[:])
	r.Feed([]byte("abc"))
	_, err := r.Next()
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
	// Cursor must be unchanged so a retry after more data works.
	r.Feed([]byte("defghij"))
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Magic != MagicResponse {
		t.Fatalf("bad magic: %v", got.Header.Magic)
	}
}

func TestBinaryReaderBadMagic(t *testing.T) {
	buf := make([]byte, BinHeaderLen)
	buf[0] = 0x42 // neither request nor response
	r := NewBinaryReader()
	r.Feed(buf)
	_, err := r.Next()
	if err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected protocol error, got %v", err)
	}
}

func TestBinaryWriterGet(t *testing.T) {
	w := NewBinaryWriter()
	buf := w.AppendGet(OpGet, "foo", 1)
	// 24-byte header, no extras, 3-byte key, no value.
	if len(buf) != BinHeaderLen+3 {
		t.Fatalf("unexpected length %d", len(buf))
	}
	if buf[0] != MagicRequest || buf[1] != OpGet {
		t.Fatalf("bad header bytes: %x %x", buf[0], buf[1])
	}
	keyLen := binary.BigEndian.Uint16(buf[2:4])
	if keyLen != 3 {
		t.Fatalf("bad key length: %d", keyLen)
	}
	bodyLen := binary.BigEndian.Uint32(buf[8:12])
	if bodyLen != 3 {
		t.Fatalf("bad body length: %d", bodyLen)
	}
	opaque := binary.BigEndian.Uint32(buf[12:16])
	if opaque != 1 {
		t.Fatalf("bad opaque: %d", opaque)
	}
	if !bytes.Equal(buf[BinHeaderLen:], []byte("foo")) {
		t.Fatalf("bad key body: %q", buf[BinHeaderLen:])
	}
}

func TestBinaryWriterStorage(t *testing.T) {
	w := NewBinaryWriter()
	buf := w.AppendStorage(OpSet, "foo", []byte("bar"), 7, 60, 100, 2)
	// Extras = 8, key = 3, value = 3. Total body = 14.
	bodyLen := binary.BigEndian.Uint32(buf[8:12])
	if bodyLen != 14 {
		t.Fatalf("bad body length: %d", bodyLen)
	}
	extrasLen := buf[4]
	if extrasLen != 8 {
		t.Fatalf("bad extras length: %d", extrasLen)
	}
	cas := binary.BigEndian.Uint64(buf[16:24])
	if cas != 100 {
		t.Fatalf("bad cas: %d", cas)
	}
	// Extras: flags + exptime.
	flags := binary.BigEndian.Uint32(buf[BinHeaderLen : BinHeaderLen+4])
	exptime := binary.BigEndian.Uint32(buf[BinHeaderLen+4 : BinHeaderLen+8])
	if flags != 7 || exptime != 60 {
		t.Fatalf("bad extras: flags=%d exptime=%d", flags, exptime)
	}
}

func TestBinaryWriterArith(t *testing.T) {
	w := NewBinaryWriter()
	buf := w.AppendArith(OpIncrement, "ctr", 3, 0, 0xFFFFFFFF, 0)
	// Extras = 20, key = 3, value = 0.
	extrasLen := buf[4]
	if extrasLen != 20 {
		t.Fatalf("bad extras length: %d", extrasLen)
	}
	delta := binary.BigEndian.Uint64(buf[BinHeaderLen : BinHeaderLen+8])
	if delta != 3 {
		t.Fatalf("bad delta: %d", delta)
	}
}
