package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// PostgreSQL numeric wire format (binary):
//
//   int16  ndigits   number of base-10000 digits
//   int16  weight    weight of the first digit (power of 10000)
//   int16  sign      0x0000 positive, 0x4000 negative, 0xC000 NaN
//   int16  dscale    display scale (number of digits after decimal point)
//   int16  digits[ndigits]  base-10000 digits, MSD first
//
// Implementing a full arbitrary-precision encoder is deferred. For v1.4.0 we
// decode into a canonical string form and require callers to pass a string
// when encoding so the server parses it in text form.

const (
	numericPositive uint16 = 0x0000
	numericNegative uint16 = 0x4000
	numericNaN      uint16 = 0xC000
	nBase                  = 10000
	nBaseDigits            = 4 // decimal digits per base-10000 digit
)

func decodeNumericBinary(src []byte) (driver.Value, error) {
	if len(src) < 8 {
		return nil, fmt.Errorf("postgres/protocol: numeric header too short (%d)", len(src))
	}
	ndigits := int16(binary.BigEndian.Uint16(src[0:2]))
	weight := int16(binary.BigEndian.Uint16(src[2:4]))
	sign := binary.BigEndian.Uint16(src[4:6])
	dscale := int16(binary.BigEndian.Uint16(src[6:8]))

	if sign == numericNaN {
		return "NaN", nil
	}
	if ndigits < 0 {
		return nil, fmt.Errorf("postgres/protocol: numeric ndigits < 0")
	}
	if int(ndigits)*2+8 > len(src) {
		return nil, fmt.Errorf("postgres/protocol: numeric truncated")
	}

	digits := make([]uint16, ndigits)
	for i := 0; i < int(ndigits); i++ {
		digits[i] = binary.BigEndian.Uint16(src[8+2*i : 10+2*i])
	}

	var b strings.Builder
	if sign == numericNegative {
		b.WriteByte('-')
	}

	// Assemble integer part.
	if weight < 0 {
		b.WriteByte('0')
	} else {
		for i := int16(0); i <= weight; i++ {
			var dg uint16
			if int(i) < len(digits) {
				dg = digits[i]
			}
			if i == 0 {
				b.WriteString(strconv.FormatUint(uint64(dg), 10))
			} else {
				fmt.Fprintf(&b, "%04d", dg)
			}
		}
	}

	if dscale > 0 {
		b.WriteByte('.')
		// Fractional part: digits with index > weight, padded with leading zeros
		// for missing positions, and a trailing window of exactly dscale digits.
		frac := make([]byte, 0, int(dscale)+nBaseDigits)
		// When weight < -1, there are leading zero groups before the first digit.
		if weight < -1 {
			pad := (-int(weight) - 1) * nBaseDigits
			frac = append(frac, []byte(strings.Repeat("0", pad))...)
		}
		// All digits are fractional when weight < 0; otherwise start after
		// the integer portion (index > weight).
		startIdx := int(weight) + 1
		if startIdx < 0 {
			startIdx = 0
		}
		for i := startIdx; i < len(digits); i++ {
			frac = append(frac, []byte(fmt.Sprintf("%04d", digits[i]))...)
		}
		// Truncate or extend to exactly dscale decimal places.
		if len(frac) < int(dscale) {
			frac = append(frac, []byte(strings.Repeat("0", int(dscale)-len(frac)))...)
		}
		b.Write(frac[:int(dscale)])
	}

	return b.String(), nil
}

func decodeNumericText(src []byte) (driver.Value, error) {
	return string(src), nil
}

// encodeNumericText just writes the textual representation. Callers should
// pass a string (or something with a String() via Valuer). We intentionally
// do not attempt binary encoding.
func encodeNumericText(dst []byte, v any) ([]byte, error) {
	v, err := resolveValuer(v)
	if err != nil {
		return nil, err
	}
	switch s := v.(type) {
	case string:
		return append(dst, s...), nil
	case []byte:
		return append(dst, s...), nil
	case float32:
		return strconv.AppendFloat(dst, float64(s), 'f', -1, 32), nil
	case float64:
		return strconv.AppendFloat(dst, s, 'f', -1, 64), nil
	}
	if n, err := asInt64(v); err == nil {
		return strconv.AppendInt(dst, n, 10), nil
	}
	return nil, fmt.Errorf("postgres/protocol: encode numeric: unsupported %T", v)
}

func init() {
	RegisterType(&TypeCodec{
		OID:          OIDNumeric,
		Name:         "numeric",
		DecodeBinary: decodeNumericBinary,
		DecodeText:   decodeNumericText,
		EncodeBinary: nil, // binary encode not implemented in v1.4.0
		EncodeText:   encodeNumericText,
		ScanType:     reflect.TypeOf(""),
	})
}
