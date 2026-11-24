package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"strconv"
)

// OIDInterval is the type OID of interval.
const OIDInterval uint32 = 1186

// -------------------- interval --------------------

// appendIntervalPart appends one signed date field ("1 year", "-2 mons",
// "+3 days") the way the server's IntervalStyle=postgres output does: a space
// before every field but the first, a "+" on a positive field that follows a
// negative one, and a plural "s" unless the value is exactly 1.
func appendIntervalPart(dst []byte, v int64, unit string, isZero, isBefore *bool) []byte {
	if v == 0 {
		return dst
	}
	if !*isZero {
		dst = append(dst, ' ')
	}
	if *isBefore && v > 0 {
		dst = append(dst, '+')
	}
	dst = strconv.AppendInt(dst, v, 10)
	dst = append(dst, ' ')
	dst = append(dst, unit...)
	if v != 1 {
		dst = append(dst, 's')
	}
	*isBefore = v < 0
	*isZero = false
	return dst
}

func decodeIntervalBinary(src []byte) (driver.Value, error) {
	if len(src) != 16 {
		return nil, fmt.Errorf("postgres/protocol: interval length = %d", len(src))
	}
	tm := int64(binary.BigEndian.Uint64(src[:8]))
	day := int64(int32(binary.BigEndian.Uint32(src[8:12])))
	mon := int64(int32(binary.BigEndian.Uint32(src[12:16])))
	// PostgreSQL 17 and later: infinity is the maximum in all three fields,
	// -infinity the minimum.
	switch {
	case tm == math.MaxInt64 && day == math.MaxInt32 && mon == math.MaxInt32:
		return "infinity", nil
	case tm == math.MinInt64 && day == math.MinInt32 && mon == math.MinInt32:
		return "-infinity", nil
	}
	year, mon := mon/12, mon%12
	// Every time field takes the sign of the whole time (truncating division).
	hour := tm / usecsPerHour
	rest := tm - hour*usecsPerHour
	mins := rest / usecsPerMinute
	rest -= mins * usecsPerMinute
	sec := rest / usecsPerSec
	usec := rest - sec*usecsPerSec

	buf := make([]byte, 0, 64)
	isZero, isBefore := true, false
	buf = appendIntervalPart(buf, year, "year", &isZero, &isBefore)
	buf = appendIntervalPart(buf, mon, "mon", &isZero, &isBefore)
	buf = appendIntervalPart(buf, day, "day", &isZero, &isBefore)
	if isZero || tm != 0 {
		if !isZero {
			buf = append(buf, ' ')
		}
		switch {
		case tm < 0:
			buf = append(buf, '-')
		case isBefore:
			buf = append(buf, '+')
		}
		if hour < 0 {
			hour = -hour
		}
		if mins < 0 {
			mins = -mins
		}
		if sec < 0 {
			sec = -sec
		}
		if usec < 0 {
			usec = -usec
		}
		buf = pad2(buf, hour)
		buf = append(buf, ':')
		buf = pad2(buf, mins)
		buf = append(buf, ':')
		buf = appendSeconds(buf, sec, usec)
	}
	return string(buf), nil
}

func init() {
	RegisterType(&TypeCodec{OID: OIDInterval, Name: "interval", DecodeBinary: decodeIntervalBinary, ScanType: reflect.TypeOf("")})
}
