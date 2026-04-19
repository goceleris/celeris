package postgres

import (
	"encoding/binary"
	"testing"
)

func TestBuildCancelRequest(t *testing.T) {
	got := buildCancelRequest(12345, 0x7FEEDCAB)
	if len(got) != 16 {
		t.Fatalf("len = %d, want 16", len(got))
	}
	if binary.BigEndian.Uint32(got[0:4]) != 16 {
		t.Errorf("length field = %d", binary.BigEndian.Uint32(got[0:4]))
	}
	if int32(binary.BigEndian.Uint32(got[4:8])) != cancelRequestCode {
		t.Errorf("code = %x", binary.BigEndian.Uint32(got[4:8]))
	}
	if int32(binary.BigEndian.Uint32(got[8:12])) != 12345 {
		t.Errorf("pid = %d", int32(binary.BigEndian.Uint32(got[8:12])))
	}
	if int32(binary.BigEndian.Uint32(got[12:16])) != 0x7FEEDCAB {
		t.Errorf("secret = %x", binary.BigEndian.Uint32(got[12:16]))
	}
}
