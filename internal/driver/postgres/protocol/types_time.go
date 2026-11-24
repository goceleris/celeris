package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// pgEpochUnix is the PostgreSQL date/time origin, 2000-01-01 00:00:00 UTC, as
// Unix seconds.
const pgEpochUnix = 946684800

// The binary date and timestamp values are plain integers (days, or
// microseconds, since the origin) and cover PostgreSQL's whole range, 4714 BC
// to 294276 AD. A time.Duration cannot hold that: it is an int64 count of
// nanoseconds and spans only about 292 years either side of the origin, so
// converting through Duration (Duration(micros)*Microsecond, or t.Sub(origin),
// which saturates) silently yields a wrong value outside roughly 1708..2292.
// Everything below therefore works in seconds and a sub-second remainder and
// builds the time.Time with time.Unix.

const (
	microsPerSecond = 1_000_000
	secondsPerDay   = 86400
	pgEpochDays     = pgEpochUnix / secondsPerDay // 10957 days from 1970-01-01 to 2000-01-01

	// The wire sentinels for infinity use the extreme integers, so a finite
	// value the encoder produces must stay strictly inside them. maxSecsFromOrigin
	// and minSecsFromOrigin bound the seconds from the origin that
	// still convert to int64 microseconds, with a second of margin for the
	// microsecond remainder.
	maxSecsFromOrigin = math.MaxInt64/microsPerSecond - 1
	minSecsFromOrigin = math.MinInt64/microsPerSecond + 1
)

// timeFromPGMicros converts microseconds since the PostgreSQL origin into a UTC
// time.Time. The split uses floor division, so a negative value (before the
// origin) gets a non-negative microsecond remainder.
func timeFromPGMicros(micros int64) time.Time {
	sec := micros / microsPerSecond
	rem := micros % microsPerSecond
	if rem < 0 {
		rem += microsPerSecond
		sec--
	}
	return time.Unix(pgEpochUnix+sec, rem*1000).UTC()
}

// pgMicrosFromTime is the inverse of timeFromPGMicros: microseconds since the
// PostgreSQL origin, truncated (floored) to a whole microsecond. It reports an
// error for an instant whose value does not fit in the wire's int64 instead of
// wrapping or saturating.
func pgMicrosFromTime(t time.Time) (int64, error) {
	u := t.Unix()
	if u < pgEpochUnix+minSecsFromOrigin || u > pgEpochUnix+maxSecsFromOrigin {
		return 0, fmt.Errorf("postgres/protocol: encode timestamp: %v is outside PostgreSQL's range", t)
	}
	// t.Nanosecond() is in [0, 1e9) for every time.Time, including those before
	// the Unix epoch, so dividing it truncates toward the earlier microsecond:
	// already the floor.
	return (u-pgEpochUnix)*microsPerSecond + int64(t.Nanosecond())/1000, nil
}

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

// -------------------- BC text forms --------------------

// bcCycleYears is a multiple of 400, the length of the Gregorian calendar
// cycle, so shifting a date by it keeps every month length and leap day.
const bcCycleYears = 4800

// splitBC reports whether the text form s ends in PostgreSQL's era suffix
// " BC" and returns s without it. PostgreSQL prints a date or timestamp before
// 1 AD as "YYYY-MM-DD BC" / "YYYY-MM-DD HH:MM:SS[.f][+tz] BC" with the year
// counted from 1 BC.
func splitBC(s string) (string, bool) {
	return strings.CutSuffix(s, " BC")
}

// bcToCycle rewrites the text form of a BC value, whose year field is N (N BC),
// to the same calendar position in an AD year that time.Parse accepts: Go's
// astronomical year is 1-N, and bcCycleYears is added to make it positive.
// 0045-02-29 BC (astronomical -44, a leap year) becomes year 4756, also a leap
// year, so a leap day still parses. fromCycle undoes the shift on the parsed
// time. N must be 1 to 4800 (PostgreSQL's earliest date is 4713 BC); there is
// no year 0 BC in the text form, so "0000 BC" is an error.
func bcToCycle(s string) (string, error) {
	i := strings.IndexByte(s, '-')
	if i < 1 || i > 5 {
		return "", errBCYear
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil || n < 1 || n > bcCycleYears {
		return "", errBCYear
	}
	y := strconv.Itoa(1 - n + bcCycleYears)
	return strings.Repeat("0", 4-len(y)) + y + s[i:], nil
}

var errBCYear = fmt.Errorf("postgres/protocol: invalid BC year")

// fromCycle maps a time parsed from bcToCycle's output back to the Go
// (astronomical) year, keeping the zone.
func fromCycle(t time.Time) time.Time {
	return t.AddDate(-bcCycleYears, 0, 0)
}

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
	return time.Unix((int64(days)+pgEpochDays)*secondsPerDay, 0).UTC(), nil
}

func decodeDateText(src []byte) (driver.Value, error) {
	s := string(src)
	switch s {
	case "infinity":
		return PGInfinity, nil
	case "-infinity":
		return PGNegInfinity, nil
	}
	if body, bc := splitBC(s); bc {
		shifted, err := bcToCycle(body)
		if err != nil {
			return nil, fmt.Errorf("postgres/protocol: date text %q: %w", src, err)
		}
		t, err := time.Parse("2006-01-02", shifted)
		if err != nil {
			return nil, fmt.Errorf("postgres/protocol: date text %q: %w", src, err)
		}
		return fromCycle(t), nil
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
	// Use the UTC calendar day: truncate to UTC midnight so a value in a
	// non-UTC zone, or with a time of day, does not move the day.
	t = t.UTC()
	y, m, d := t.Date()
	u := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
	// u is an exact multiple of a day; the floor division is defensive only.
	days := u / secondsPerDay
	if u%secondsPerDay < 0 {
		days--
	}
	days -= pgEpochDays
	// The int32 day count reaches about +-5.8 million years; the extreme
	// values are the infinity sentinels, so keep clear of them.
	if days <= int64(pgDateNegInfinity) || days >= int64(pgDateInfinity) {
		return nil, fmt.Errorf("postgres/protocol: encode date: %v is outside PostgreSQL's range", t)
	}
	w := uint32(int32(days))
	return append(dst, byte(w>>24), byte(w>>16), byte(w>>8), byte(w)), nil
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
	return timeFromPGMicros(micros), nil
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
	body, bc := splitBC(s)
	if bc {
		shifted, err := bcToCycle(body)
		if err != nil {
			return nil, fmt.Errorf("postgres/protocol: timestamp text %q: %w", src, err)
		}
		body = shifted
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, body, time.UTC); err == nil {
			if bc {
				t = fromCycle(t)
			}
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
	body, bc := splitBC(s)
	if bc {
		shifted, err := bcToCycle(body)
		if err != nil {
			return nil, fmt.Errorf("postgres/protocol: timestamptz text %q: %w", src, err)
		}
		body = shifted
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05-07",
	} {
		if t, err := time.Parse(layout, body); err == nil {
			t = t.UTC()
			if bc {
				t = fromCycle(t)
			}
			return t, nil
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
	micros, err := pgMicrosFromTime(t)
	if err != nil {
		return nil, err
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
