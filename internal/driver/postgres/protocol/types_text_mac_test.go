package protocol

import "testing"

func TestDecodeMacaddrBinary(t *testing.T) {
	for _, c := range []struct {
		oid  uint32
		src  []byte
		want string
	}{
		{OIDMacaddr, []byte{0, 0, 0, 0, 0, 0}, "00:00:00:00:00:00"},
		{OIDMacaddr, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "ff:ff:ff:ff:ff:ff"},
		{OIDMacaddr, []byte{0x08, 0x00, 0x2b, 0x01, 0x02, 0x03}, "08:00:2b:01:02:03"},
		{OIDMacaddr8, []byte{0x08, 0x00, 0x2b, 0x01, 0x02, 0x03, 0x04, 0x05}, "08:00:2b:01:02:03:04:05"},
		{OIDMacaddr8, []byte{0xab, 0xcd, 0xef, 0, 1, 2, 3, 0xff}, "ab:cd:ef:00:01:02:03:ff"},
	} {
		v, err := LookupOID(c.oid).DecodeBinary(c.src)
		if err != nil || v != c.want {
			t.Errorf("oid %d % x: got %v err %v, want %q", c.oid, c.src, v, err, c.want)
		}
	}
	if _, err := decodeMacaddrBinary(make([]byte, 8)); err == nil {
		t.Error("an 8-byte macaddr must be an error")
	}
	if _, err := decodeMacaddr8Binary(make([]byte, 6)); err == nil {
		t.Error("a 6-byte macaddr8 must be an error")
	}
}
