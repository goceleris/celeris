package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"sync"
)

// PostgreSQL type OIDs. These values are stable across PostgreSQL versions
// and are defined in src/include/catalog/pg_type.dat in the server source.
const (
	OIDBool        uint32 = 16
	OIDBytea       uint32 = 17
	OIDInt8        uint32 = 20
	OIDInt2        uint32 = 21
	OIDInt4        uint32 = 23
	OIDText        uint32 = 25
	OIDJSON        uint32 = 114
	OIDFloat4      uint32 = 700
	OIDFloat8      uint32 = 701
	OIDUnknown     uint32 = 705
	OIDVarchar     uint32 = 1043
	OIDDate        uint32 = 1082
	OIDTimestamp   uint32 = 1114
	OIDTimestamptz uint32 = 1184
	OIDNumeric     uint32 = 1700
	OIDUUID        uint32 = 2950
	OIDJSONB       uint32 = 3802

	OIDBoolArr    uint32 = 1000
	OIDByteaArr   uint32 = 1001
	OIDInt2Arr    uint32 = 1005
	OIDInt4Arr    uint32 = 1007
	OIDTextArr    uint32 = 1009
	OIDInt8Arr    uint32 = 1016
	OIDFloat4Arr  uint32 = 1021
	OIDFloat8Arr  uint32 = 1022
	OIDVarcharArr uint32 = 1015
	OIDUUIDArr    uint32 = 2951
)

// Format codes used in Bind/Describe messages.
const (
	FormatText   int16 = 0
	FormatBinary int16 = 1
)

// TypeCodec encodes and decodes a single PostgreSQL type. The Encode
// functions follow the append convention: they append encoded bytes to dst
// and return the (possibly reallocated) slice. Encoders that receive a
// driver.Valuer will call Value() first and re-dispatch based on the
// resulting value. A nil value is the caller's responsibility (the
// framing layer writes a length of -1 for NULLs); codecs never see nil.
type TypeCodec struct {
	OID          uint32
	Name         string
	DecodeText   func(src []byte) (driver.Value, error)
	DecodeBinary func(src []byte) (driver.Value, error)
	EncodeText   func(dst []byte, v any) ([]byte, error)
	EncodeBinary func(dst []byte, v any) ([]byte, error)
	ScanType     reflect.Type
}

var (
	codecsMu sync.RWMutex
	codecs   = map[uint32]*TypeCodec{}
)

// RegisterType registers a custom type codec. Later registrations override
// earlier ones for the same OID. Safe to call at init() time or at runtime.
func RegisterType(c *TypeCodec) {
	if c == nil {
		panic("postgres/protocol: RegisterType(nil)")
	}
	codecsMu.Lock()
	codecs[c.OID] = c
	codecsMu.Unlock()
}

// LookupOID returns the codec registered for oid, or nil if none is
// registered.
func LookupOID(oid uint32) *TypeCodec {
	codecsMu.RLock()
	c := codecs[oid]
	codecsMu.RUnlock()
	return c
}

// resolveValuer calls v.Value() if v implements driver.Valuer, repeatedly
// until a non-Valuer is returned. It protects against cycles with a hard
// iteration cap.
func resolveValuer(v any) (any, error) {
	for i := 0; i < 8; i++ {
		valuer, ok := v.(driver.Valuer)
		if !ok {
			return v, nil
		}
		next, err := valuer.Value()
		if err != nil {
			return nil, err
		}
		v = next
	}
	return nil, errors.New("postgres/protocol: driver.Valuer recursion too deep")
}

// -------------------- bool --------------------

func decodeBoolBinary(src []byte) (driver.Value, error) {
	if len(src) != 1 {
		return nil, fmt.Errorf("postgres/protocol: bool binary length = %d", len(src))
	}
	return src[0] != 0, nil
}

func decodeBoolText(src []byte) (driver.Value, error) {
	switch string(src) {
	case "t", "true", "y", "yes", "on", "1":
		return true, nil
	case "f", "false", "n", "no", "off", "0":
		return false, nil
	}
	return nil, fmt.Errorf("postgres/protocol: bool text = %q", src)
}

func encodeBoolBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode bool: unsupported %T", v)
	}
	if b {
		return append(dst, 1), nil
	}
	return append(dst, 0), nil
}

func encodeBoolText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode bool: unsupported %T", v)
	}
	if b {
		return append(dst, 't'), nil
	}
	return append(dst, 'f'), nil
}

// -------------------- int2/int4/int8 --------------------

func decodeInt2Binary(src []byte) (driver.Value, error) {
	if len(src) != 2 {
		return nil, fmt.Errorf("postgres/protocol: int2 length = %d", len(src))
	}
	return int64(int16(binary.BigEndian.Uint16(src))), nil
}

func decodeInt4Binary(src []byte) (driver.Value, error) {
	if len(src) != 4 {
		return nil, fmt.Errorf("postgres/protocol: int4 length = %d", len(src))
	}
	return int64(int32(binary.BigEndian.Uint32(src))), nil
}

func decodeInt8Binary(src []byte) (driver.Value, error) {
	if len(src) != 8 {
		return nil, fmt.Errorf("postgres/protocol: int8 length = %d", len(src))
	}
	return int64(binary.BigEndian.Uint64(src)), nil
}

func decodeIntText(src []byte) (driver.Value, error) {
	n, err := strconv.ParseInt(string(src), 10, 64)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// asInt64 coerces common numeric Go types (after Valuer resolution) to int64.
func asInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint:
		return int64(n), nil
	case uint8:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		if n > math.MaxInt64 {
			return 0, fmt.Errorf("postgres/protocol: uint64 %d overflows int64", n)
		}
		return int64(n), nil
	}
	return 0, fmt.Errorf("postgres/protocol: cannot encode %T as integer", v)
}

func encodeInt2Binary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	n, err := asInt64(v)
	if err != nil {
		return nil, err
	}
	if n < math.MinInt16 || n > math.MaxInt16 {
		return nil, fmt.Errorf("postgres/protocol: int2 overflow %d", n)
	}
	u := uint16(int16(n))
	return append(dst, byte(u>>8), byte(u)), nil
}

func encodeInt4Binary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	n, err := asInt64(v)
	if err != nil {
		return nil, err
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return nil, fmt.Errorf("postgres/protocol: int4 overflow %d", n)
	}
	u := uint32(int32(n))
	return append(dst, byte(u>>24), byte(u>>16), byte(u>>8), byte(u)), nil
}

func encodeInt8Binary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	n, err := asInt64(v)
	if err != nil {
		return nil, err
	}
	u := uint64(n)
	return append(dst, byte(u>>56), byte(u>>48), byte(u>>40), byte(u>>32),
		byte(u>>24), byte(u>>16), byte(u>>8), byte(u)), nil
}

func encodeIntText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	n, err := asInt64(v)
	if err != nil {
		return nil, err
	}
	return strconv.AppendInt(dst, n, 10), nil
}

// -------------------- float4/float8 --------------------

func decodeFloat4Binary(src []byte) (driver.Value, error) {
	if len(src) != 4 {
		return nil, fmt.Errorf("postgres/protocol: float4 length = %d", len(src))
	}
	return float64(math.Float32frombits(binary.BigEndian.Uint32(src))), nil
}

func decodeFloat8Binary(src []byte) (driver.Value, error) {
	if len(src) != 8 {
		return nil, fmt.Errorf("postgres/protocol: float8 length = %d", len(src))
	}
	return math.Float64frombits(binary.BigEndian.Uint64(src)), nil
}

func decodeFloatText(src []byte) (driver.Value, error) {
	s := string(src)
	// PostgreSQL uses "NaN", "Infinity", "-Infinity" textual forms.
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		return math.Inf(1), nil
	case "-Infinity":
		return math.Inf(-1), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func asFloat64(v any) (float64, error) {
	switch f := v.(type) {
	case float32:
		return float64(f), nil
	case float64:
		return f, nil
	}
	if n, err := asInt64(v); err == nil {
		return float64(n), nil
	}
	return 0, fmt.Errorf("postgres/protocol: cannot encode %T as float", v)
}

func encodeFloat4Binary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	f, err := asFloat64(v)
	if err != nil {
		return nil, err
	}
	u := math.Float32bits(float32(f))
	return append(dst, byte(u>>24), byte(u>>16), byte(u>>8), byte(u)), nil
}

func encodeFloat8Binary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	f, err := asFloat64(v)
	if err != nil {
		return nil, err
	}
	u := math.Float64bits(f)
	return append(dst, byte(u>>56), byte(u>>48), byte(u>>40), byte(u>>32),
		byte(u>>24), byte(u>>16), byte(u>>8), byte(u)), nil
}

func encodeFloatText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	f, err := asFloat64(v)
	if err != nil {
		return nil, err
	}
	switch {
	case math.IsNaN(f):
		return append(dst, "NaN"...), nil
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...), nil
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...), nil
	}
	return strconv.AppendFloat(dst, f, 'g', -1, 64), nil
}

// -------------------- text / varchar --------------------

func decodeStringBinary(src []byte) (driver.Value, error) {
	return string(src), nil
}

func encodeStringBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	switch s := v.(type) {
	case string:
		return append(dst, s...), nil
	case []byte:
		return append(dst, s...), nil
	}
	return nil, fmt.Errorf("postgres/protocol: encode text: unsupported %T", v)
}

// -------------------- bytea --------------------

func decodeByteaBinary(src []byte) (driver.Value, error) {
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

func decodeByteaText(src []byte) (driver.Value, error) {
	// Text format is "\x" + hex. Support it for completeness.
	if len(src) >= 2 && src[0] == '\\' && src[1] == 'x' {
		hexSrc := src[2:]
		if len(hexSrc)%2 != 0 {
			return nil, fmt.Errorf("postgres/protocol: bytea hex odd length")
		}
		out := make([]byte, len(hexSrc)/2)
		for i := 0; i < len(out); i++ {
			hi, err := fromHexNibble(hexSrc[2*i])
			if err != nil {
				return nil, err
			}
			lo, err := fromHexNibble(hexSrc[2*i+1])
			if err != nil {
				return nil, err
			}
			out[i] = hi<<4 | lo
		}
		return out, nil
	}
	// Legacy "escape" format not supported.
	return nil, fmt.Errorf("postgres/protocol: bytea non-hex text format unsupported")
}

func fromHexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return 10 + c - 'a', nil
	case c >= 'A' && c <= 'F':
		return 10 + c - 'A', nil
	}
	return 0, fmt.Errorf("postgres/protocol: invalid hex nibble %q", c)
}

func encodeByteaBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode bytea: unsupported %T", v)
	}
	return append(dst, b...), nil
}

func encodeByteaText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("postgres/protocol: encode bytea: unsupported %T", v)
	}
	dst = append(dst, '\\', 'x')
	const hex = "0123456789abcdef"
	for _, x := range b {
		dst = append(dst, hex[x>>4], hex[x&0xf])
	}
	return dst, nil
}

// -------------------- uuid --------------------

func decodeUUIDBinary(src []byte) (driver.Value, error) {
	if len(src) != 16 {
		return nil, fmt.Errorf("postgres/protocol: uuid length = %d", len(src))
	}
	out := make([]byte, 16)
	copy(out, src)
	return out, nil
}

func decodeUUIDText(src []byte) (driver.Value, error) {
	// 8-4-4-4-12 form.
	if len(src) != 36 {
		return nil, fmt.Errorf("postgres/protocol: uuid text length = %d", len(src))
	}
	out := make([]byte, 16)
	j := 0
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if src[i] != '-' {
				return nil, fmt.Errorf("postgres/protocol: uuid separator at %d = %q", i, src[i])
			}
			continue
		}
		hi, err := fromHexNibble(src[i])
		if err != nil {
			return nil, err
		}
		lo, err := fromHexNibble(src[i+1])
		if err != nil {
			return nil, err
		}
		out[j] = hi<<4 | lo
		j++
		i++
	}
	return out, nil
}

func encodeUUIDBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	switch u := v.(type) {
	case []byte:
		if len(u) != 16 {
			return nil, fmt.Errorf("postgres/protocol: uuid encode len=%d", len(u))
		}
		return append(dst, u...), nil
	case [16]byte:
		return append(dst, u[:]...), nil
	}
	return nil, fmt.Errorf("postgres/protocol: encode uuid: unsupported %T", v)
}

// -------------------- JSON / JSONB --------------------

func decodeJSONBinary(src []byte) (driver.Value, error) {
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

func decodeJSONBBinary(src []byte) (driver.Value, error) {
	if len(src) < 1 {
		return nil, fmt.Errorf("postgres/protocol: jsonb empty")
	}
	if src[0] != 1 {
		return nil, fmt.Errorf("postgres/protocol: jsonb version %d unsupported", src[0])
	}
	out := make([]byte, len(src)-1)
	copy(out, src[1:])
	return out, nil
}

func encodeJSONBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	switch b := v.(type) {
	case []byte:
		return append(dst, b...), nil
	case string:
		return append(dst, b...), nil
	}
	return nil, fmt.Errorf("postgres/protocol: encode json: unsupported %T", v)
}

func encodeJSONBBinary(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	dst = append(dst, 1) // jsonb format version
	switch b := v.(type) {
	case []byte:
		return append(dst, b...), nil
	case string:
		return append(dst, b...), nil
	}
	return nil, fmt.Errorf("postgres/protocol: encode jsonb: unsupported %T", v)
}

func init() {
	RegisterType(&TypeCodec{
		OID:          OIDBool,
		Name:         "bool",
		DecodeBinary: decodeBoolBinary,
		DecodeText:   decodeBoolText,
		EncodeBinary: encodeBoolBinary,
		EncodeText:   encodeBoolText,
		ScanType:     reflect.TypeOf(false),
	})
	RegisterType(&TypeCodec{
		OID:          OIDInt2,
		Name:         "int2",
		DecodeBinary: decodeInt2Binary,
		DecodeText:   decodeIntText,
		EncodeBinary: encodeInt2Binary,
		EncodeText:   encodeIntText,
		ScanType:     reflect.TypeOf(int64(0)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDInt4,
		Name:         "int4",
		DecodeBinary: decodeInt4Binary,
		DecodeText:   decodeIntText,
		EncodeBinary: encodeInt4Binary,
		EncodeText:   encodeIntText,
		ScanType:     reflect.TypeOf(int64(0)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDInt8,
		Name:         "int8",
		DecodeBinary: decodeInt8Binary,
		DecodeText:   decodeIntText,
		EncodeBinary: encodeInt8Binary,
		EncodeText:   encodeIntText,
		ScanType:     reflect.TypeOf(int64(0)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDFloat4,
		Name:         "float4",
		DecodeBinary: decodeFloat4Binary,
		DecodeText:   decodeFloatText,
		EncodeBinary: encodeFloat4Binary,
		EncodeText:   encodeFloatText,
		ScanType:     reflect.TypeOf(float64(0)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDFloat8,
		Name:         "float8",
		DecodeBinary: decodeFloat8Binary,
		DecodeText:   decodeFloatText,
		EncodeBinary: encodeFloat8Binary,
		EncodeText:   encodeFloatText,
		ScanType:     reflect.TypeOf(float64(0)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDText,
		Name:         "text",
		DecodeBinary: decodeStringBinary,
		DecodeText:   decodeStringBinary,
		EncodeBinary: encodeStringBinary,
		EncodeText:   encodeStringBinary,
		ScanType:     reflect.TypeOf(""),
	})
	RegisterType(&TypeCodec{
		OID:          OIDVarchar,
		Name:         "varchar",
		DecodeBinary: decodeStringBinary,
		DecodeText:   decodeStringBinary,
		EncodeBinary: encodeStringBinary,
		EncodeText:   encodeStringBinary,
		ScanType:     reflect.TypeOf(""),
	})
	RegisterType(&TypeCodec{
		OID:          OIDBytea,
		Name:         "bytea",
		DecodeBinary: decodeByteaBinary,
		DecodeText:   decodeByteaText,
		EncodeBinary: encodeByteaBinary,
		EncodeText:   encodeByteaText,
		ScanType:     reflect.TypeOf([]byte(nil)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDUUID,
		Name:         "uuid",
		DecodeBinary: decodeUUIDBinary,
		DecodeText:   decodeUUIDText,
		EncodeBinary: encodeUUIDBinary,
		EncodeText:   nil, // uuids go over binary in our driver
		ScanType:     reflect.TypeOf([]byte(nil)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDJSON,
		Name:         "json",
		DecodeBinary: decodeJSONBinary,
		DecodeText:   decodeJSONBinary,
		EncodeBinary: encodeJSONBinary,
		EncodeText:   encodeJSONBinary,
		ScanType:     reflect.TypeOf([]byte(nil)),
	})
	RegisterType(&TypeCodec{
		OID:          OIDJSONB,
		Name:         "jsonb",
		DecodeBinary: decodeJSONBBinary,
		DecodeText:   decodeJSONBinary, // text jsonb has no version byte
		EncodeBinary: encodeJSONBBinary,
		EncodeText:   encodeJSONBinary,
		ScanType:     reflect.TypeOf([]byte(nil)),
	})
}
