package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"time"
)

// pgEpoch is the PostgreSQL date/time origin: 2000-01-01 00:00:00 UTC.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// PGInfinity and PGNegInfinity are sentinel time.Time values returned when
// PostgreSQL sends infinity/−infinity for timestamp or date columns. Callers
// should check for these with time.Equal before arithmetic.
var (
	PGInfinity    = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	PGNegInfinity = time.Date(-4713, 1, 1, 0, 0, 0, 0, time.UTC)
)

// PostgreSQL binary sentinel values for infinity/−infinity.
const (
	pgTimestampInfinity    = int64(math.MaxInt64)
	pgTimestampNegInfinity = int64(math.MinInt64)
	pgDateInfinity         = int32(math.MaxInt32)
	pgDateNegInfinity      = int32(math.MinInt32)
)

// -------------------- date --------------------

func decodeDateBinary(src []byte) (driver.Value, error) {
	if len(src) != 4 {
		return nil, fmt.Errorf("postgres/protocol: date length = %d", len(src))
	}
	days := int32(binary.BigEndian.Uint32(src))
	switch days {
	case pgDateInfinity:
		return PGInfinity, nil
	case pgDateNegInfinity:
		return PGNegInfinity, nil
	}
	return pgEpoch.AddDate(0, 0, int(days)), nil
}

func decodeDateText(src []byte) (driver.Value, error) {
	s := string(src)
	switch s {
	case "infinity":
		return PGInfinity, nil
	case "-infinity":
		return PGNegInfinity, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func encodeDateBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	t, ok := v.(time.Time)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode date: unsupported %T", v)
	}
	// Truncate to UTC midnight before computing the day delta so we don't
	// round to the "wrong" day for values in non-UTC zones.
	t = t.UTC()
	y, m, d := t.Date()
	t = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	// Go's integer division truncates toward zero — for negative deltas
	// (dates before pgEpoch = 2000-01-01 UTC) with a non-zero remainder that
	// produces the mathematically-wrong result. We already truncate t to
	// UTC midnight above so the remainder SHOULD be zero, but we apply the
	// floor correction defensively in case a future refactor feeds in a
	// sub-day offset. int64 → int32 truncation is safe: the int32 range of
	// days covers roughly ±5.8M years, which dwarfs any representable
	// time.Time.
	delta := t.Sub(pgEpoch)
	day := 24 * time.Hour
	days := int64(delta / day)
	if rem := delta % day; rem < 0 {
		days--
	}
	u := uint32(int32(days))
	return append(dst, byte(u>>24), byte(u>>16), byte(u>>8), byte(u)), nil
}

func encodeDateText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	t, ok := v.(time.Time)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode date: unsupported %T", v)
	}
	return append(dst, t.UTC().Format("2006-01-02")...), nil
}

// -------------------- timestamp / timestamptz --------------------

// decodeTimestampBinary interprets the wire value as microseconds since
// pgEpoch in UTC. Note PostgreSQL also supports an integer/float datetime
// compile-time switch, but all servers since 10.0 always use integer
// microseconds — the float code path is dead.
func decodeTimestampBinary(src []byte) (driver.Value, error) {
	if len(src) != 8 {
		return nil, fmt.Errorf("postgres/protocol: timestamp length = %d", len(src))
	}
	micros := int64(binary.BigEndian.Uint64(src))
	switch micros {
	case pgTimestampInfinity:
		return PGInfinity, nil
	case pgTimestampNegInfinity:
		return PGNegInfinity, nil
	}
	return pgEpoch.Add(time.Duration(micros) * time.Microsecond), nil
}

func decodeTimestampText(src []byte) (driver.Value, error) {
	s := string(src)
	switch s {
	case "infinity":
		return PGInfinity, nil
	case "-infinity":
		return PGNegInfinity, nil
	}
	// Text form lacks timezone; parse as UTC.
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("postgres/protocol: timestamp text %q", src)
}

func decodeTimestamptzText(src []byte) (driver.Value, error) {
	s := string(src)
	switch s {
	case "infinity":
		return PGInfinity, nil
	case "-infinity":
		return PGNegInfinity, nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05-07",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return nil, fmt.Errorf("postgres/protocol: timestamptz text %q", src)
}

func encodeTimestampBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	t, ok := v.(time.Time)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode timestamp: unsupported %T", v)
	}
	d := t.UTC().Sub(pgEpoch)
	micros := int64(d / time.Microsecond)
	if d < 0 && d%time.Microsecond != 0 {
		micros--
	}
	u := uint64(micros)
	return append(dst, byte(u>>56), byte(u>>48), byte(u>>40), byte(u>>32),
		byte(u>>24), byte(u>>16), byte(u>>8), byte(u)), nil
}

func encodeTimestampText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	t, ok := v.(time.Time)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode timestamp: unsupported %T", v)
	}
	return append(dst, t.UTC().Format("2006-01-02 15:04:05.999999")...), nil
}

func encodeTimestamptzText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	t, ok := v.(time.Time)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode timestamptz: unsupported %T", v)
	}
	return append(dst, t.UTC().Format("2006-01-02 15:04:05.999999Z07:00")...), nil
}

func init() {
	timeType := reflect.TypeOf(time.Time{})
	RegisterType(&TypeCodec{
		OID:          OIDDate,
		Name:         "date",
		DecodeBinary: decodeDateBinary,
		DecodeText:   decodeDateText,
		EncodeBinary: encodeDateBinary,
		EncodeText:   encodeDateText,
		ScanType:     timeType,
	})
	RegisterType(&TypeCodec{
		OID:          OIDTimestamp,
		Name:         "timestamp",
		DecodeBinary: decodeTimestampBinary,
		DecodeText:   decodeTimestampText,
		EncodeBinary: encodeTimestampBinary,
		EncodeText:   encodeTimestampText,
		ScanType:     timeType,
	})
	RegisterType(&TypeCodec{
		OID:          OIDTimestamptz,
		Name:         "timestamptz",
		DecodeBinary: decodeTimestampBinary, // wire format is identical to timestamp
		DecodeText:   decodeTimestamptzText,
		EncodeBinary: encodeTimestampBinary,
		EncodeText:   encodeTimestamptzText,
		ScanType:     timeType,
	})
}
