package redis

import "strings"

// Slot computes the Redis Cluster hash slot for a key. If the key contains a
// hash tag (i.e. a substring enclosed in braces like "{tag}"), only the tag
// portion is hashed — this lets callers colocate related keys on the same
// slot.
func Slot(key string) uint16 {
	if i := strings.Index(key, "{"); i >= 0 {
		if j := strings.Index(key[i+1:], "}"); j > 0 {
			key = key[i+1 : i+1+j]
		}
	}
	return crc16(key) % 16384
}

// crc16 computes the CRC-CCITT (XMODEM) checksum used by Redis Cluster.
// Polynomial: x^16 + x^12 + x^5 + 1 (0x1021).
func crc16(data string) uint16 {
	var crc uint16
	for i := 0; i < len(data); i++ {
		crc ^= uint16(data[i]) << 8
		for j := 0; j < 8; j++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
