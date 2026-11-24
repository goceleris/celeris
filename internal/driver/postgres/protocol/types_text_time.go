package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
)

// OIDTime is the type OID of time (without time zone).
const OIDTime uint32 = 1083

const (
	usecsPerSec    = int64(1000000)
	usecsPerMinute = 60 * usecsPerSec
	usecsPerHour   = 60 * usecsPerMinute
	usecsPerDay    = 24 * usecsPerHour
)

func pad2(dst []byte, n int64) []byte {
	if n < 10 {
		dst = append(dst, '0')
	}
	return strconv.AppendInt(dst, n, 10)
}

// appendSeconds appends SS and, when usec is not zero, the fraction with its
// trailing zeros removed (the server's AppendSeconds with six digits).
func appendSeconds(dst []byte, sec, usec int64) []byte {
	dst = pad2(dst, sec)
	if usec == 0 {
		return dst
	}
	var frac [6]byte
	for i := 5; i >= 0; i-- {
		frac[i] = byte('0' + usec%10)
		usec /= 10
	}
	n := 6
	for n > 0 && frac[n-1] == '0' {
		n--
	}
	dst = append(dst, '.')
	return append(dst, frac[:n]...)
}

// appendTimeOfDay appends HH:MM:SS[.ffffff] for a count of microseconds since
// midnight (86400000000, "24:00:00", is the one value past the last instant).
func appendTimeOfDay(dst []byte, t int64) []byte {
	dst = pad2(dst, t/usecsPerHour)
	dst = append(dst, ':')
	dst = pad2(dst, t%usecsPerHour/usecsPerMinute)
	dst = append(dst, ':')
	return appendSeconds(dst, t%usecsPerMinute/usecsPerSec, t%usecsPerSec)
}

// -------------------- time --------------------

func decodeTimeBinary(src []byte) (driver.Value, error) {
	if len(src) != 8 {
		return nil, fmt.Errorf("postgres/protocol: time length = %d", len(src))
	}
	t := int64(binary.BigEndian.Uint64(src))
	if t < 0 || t > usecsPerDay {
		return nil, fmt.Errorf("postgres/protocol: time out of range: %d microseconds", t)
	}
	return string(appendTimeOfDay(make([]byte, 0, 15), t)), nil
}

func init() {
	RegisterType(&TypeCodec{OID: OIDTime, Name: "time", DecodeBinary: decodeTimeBinary, ScanType: reflect.TypeOf("")})
}
