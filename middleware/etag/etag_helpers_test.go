package etag

import "encoding/hex"

// formatCRC32ETag formats a CRC-32 checksum as an ETag value using a stack
// buffer. Used by tests to compute expected ETag values.
func formatCRC32ETag(checksum uint32, weak bool) string {
	var buf [14]byte
	if weak {
		buf[0] = 'W'
		buf[1] = '/'
		buf[2] = '"'
		hex.Encode(buf[3:11], []byte{byte(checksum >> 24), byte(checksum >> 16), byte(checksum >> 8), byte(checksum)})
		buf[11] = '"'
		return string(buf[:12])
	}
	buf[0] = '"'
	hex.Encode(buf[1:9], []byte{byte(checksum >> 24), byte(checksum >> 16), byte(checksum >> 8), byte(checksum)})
	buf[9] = '"'
	return string(buf[:10])
}
