package protocol

import "testing"

func TestDecodeOidBinary(t *testing.T) {
	for _, c := range []struct {
		src  []byte
		want int64
	}{
		{[]byte{0, 0, 0, 0}, 0},
		{[]byte{0, 0, 0, 26}, 26},
		{[]byte{0, 0, 0x40, 0}, 16384},
		{[]byte{0x7f, 0xff, 0xff, 0xff}, 2147483647},
		{[]byte{0x80, 0, 0, 0}, 2147483648},
		{[]byte{0xff, 0xff, 0xff, 0xff}, 4294967295},
	} {
		v, err := LookupOID(OIDOid).DecodeBinary(c.src)
		if err != nil || v != c.want {
			t.Errorf("% x: got %v err %v, want %d", c.src, v, err, c.want)
		}
	}
	if _, err := decodeOidBinary([]byte{0, 0, 1}); err == nil {
		t.Error("a 3-byte oid must be an error")
	}
	if LookupOID(OIDOid).DecodeText != nil {
		t.Error("oid has no text decoder: the simple protocol's text is returned as it is")
	}
}
