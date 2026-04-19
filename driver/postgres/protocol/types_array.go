package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"reflect"
)

// Binary array wire format (one-dimensional is the common case we support):
//
//   int32 ndim         number of dimensions
//   int32 hasNulls     0 or 1
//   int32 elementOID
//   per dim:
//     int32 length
//     int32 lowerBound (usually 1)
//   per element:
//     int32 length (-1 => NULL)
//     bytes length   (omitted when length == -1)
//
// Multi-dimensional arrays are decoded into a flat slice with dimension
// metadata lost; the driver exposes them as []driver.Value. Callers that
// need the dimension shape can decode manually via DecodeArrayBinary.

// DecodedArray is a lightweight view over a decoded array value.
type DecodedArray struct {
	ElementOID uint32
	Dims       []int32 // dim lengths
	Elements   []driver.Value
}

// DecodeArrayBinary decodes a PostgreSQL binary array into a DecodedArray.
// Elements are decoded by looking up the element OID in the codec table.
func DecodeArrayBinary(src []byte) (*DecodedArray, error) {
	if len(src) < 12 {
		return nil, fmt.Errorf("postgres/protocol: array header too short (%d)", len(src))
	}
	ndim := int32(binary.BigEndian.Uint32(src[0:4]))
	// flags (hasNulls) unused at decode time.
	elemOID := binary.BigEndian.Uint32(src[8:12])
	if ndim < 0 {
		return nil, fmt.Errorf("postgres/protocol: array ndim negative")
	}
	pos := 12
	dims := make([]int32, ndim)
	total := int32(1)
	for i := int32(0); i < ndim; i++ {
		if pos+8 > len(src) {
			return nil, fmt.Errorf("postgres/protocol: array dim header truncated")
		}
		length := int32(binary.BigEndian.Uint32(src[pos : pos+4]))
		// lower bound at pos+4..pos+8 ignored
		if length < 0 {
			return nil, fmt.Errorf("postgres/protocol: array dim length negative")
		}
		dims[i] = length
		total *= length
		pos += 8
	}

	codec := LookupOID(elemOID)

	out := &DecodedArray{ElementOID: elemOID, Dims: dims, Elements: make([]driver.Value, 0, total)}

	// Empty array: ndim=0 is valid and means no elements, no dim header.
	if ndim == 0 {
		return out, nil
	}

	for i := int32(0); i < total; i++ {
		if pos+4 > len(src) {
			return nil, fmt.Errorf("postgres/protocol: array element length truncated")
		}
		length := int32(binary.BigEndian.Uint32(src[pos : pos+4]))
		pos += 4
		if length == -1 {
			out.Elements = append(out.Elements, nil)
			continue
		}
		if length < 0 {
			return nil, fmt.Errorf("postgres/protocol: array element length %d", length)
		}
		if pos+int(length) > len(src) {
			return nil, fmt.Errorf("postgres/protocol: array element body truncated")
		}
		elemBytes := src[pos : pos+int(length)]
		pos += int(length)
		if codec == nil || codec.DecodeBinary == nil {
			// Best-effort: return raw bytes when no decoder is registered.
			cpy := make([]byte, len(elemBytes))
			copy(cpy, elemBytes)
			out.Elements = append(out.Elements, cpy)
			continue
		}
		v, err := codec.DecodeBinary(elemBytes)
		if err != nil {
			return nil, fmt.Errorf("postgres/protocol: array elem: %w", err)
		}
		out.Elements = append(out.Elements, v)
	}

	return out, nil
}

// EncodeArrayBinary encodes a 1-D slice of values as a PostgreSQL binary
// array with the given element OID. Nil elements are encoded with length -1.
func EncodeArrayBinary(dst []byte, elementOID uint32, elements []any) ([]byte, error) {
	codec := LookupOID(elementOID)
	if codec == nil || codec.EncodeBinary == nil {
		return nil, fmt.Errorf("postgres/protocol: no binary encoder for OID %d", elementOID)
	}

	hasNulls := uint32(0)
	for _, e := range elements {
		if e == nil {
			hasNulls = 1
			break
		}
	}

	ndim := uint32(1)
	if len(elements) == 0 {
		ndim = 0
	}

	// Header.
	dst = appendU32(dst, ndim)
	dst = appendU32(dst, hasNulls)
	dst = appendU32(dst, elementOID)

	// Dimension header for the single dimension when non-empty.
	if ndim == 1 {
		dst = appendU32(dst, uint32(len(elements)))
		dst = appendU32(dst, 1) // lower bound
	}

	// Elements.
	for _, e := range elements {
		if e == nil {
			dst = appendU32(dst, 0xFFFFFFFF) // -1
			continue
		}
		// Reserve 4 bytes for the element length, fill afterwards.
		lenOffset := len(dst)
		dst = append(dst, 0, 0, 0, 0)
		before := len(dst)
		var err error
		dst, err = codec.EncodeBinary(dst, e)
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(dst[lenOffset:lenOffset+4], uint32(len(dst)-before))
	}

	return dst, nil
}

func appendU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// decodeArrayBinaryValue is the TypeCodec.DecodeBinary adapter.
func decodeArrayBinaryValue(src []byte) (driver.Value, error) {
	arr, err := DecodeArrayBinary(src)
	if err != nil {
		return nil, err
	}
	// Expose as []driver.Value for uniformity with database/sql.
	return arr.Elements, nil
}

// encodeArrayBinaryValue is the TypeCodec.EncodeBinary adapter. It accepts
// either []any or a typed slice (reflect used as the slow path).
func encodeArrayBinaryValue(elementOID uint32) func(dst []byte, v any) ([]byte, error) {
	return func(dst []byte, v any) ([]byte, error) {
		v, err := resolveValuer(v)
		if err != nil {
			return nil, err
		}
		switch slc := v.(type) {
		case []any:
			return EncodeArrayBinary(dst, elementOID, slc)
		case []driver.Value:
			// []driver.Value has the same memory layout as []any; copy through.
			els := make([]any, len(slc))
			for i, x := range slc {
				els[i] = x
			}
			return EncodeArrayBinary(dst, elementOID, els)
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return nil, fmt.Errorf("postgres/protocol: encode array: unsupported %T", v)
		}
		els := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			it := rv.Index(i)
			if it.Kind() == reflect.Interface && it.IsNil() {
				els[i] = nil
			} else {
				els[i] = it.Interface()
			}
		}
		return EncodeArrayBinary(dst, elementOID, els)
	}
}

func registerArrayCodec(oid, elementOID uint32, name string) {
	RegisterType(&TypeCodec{
		OID:          oid,
		Name:         name,
		DecodeBinary: decodeArrayBinaryValue,
		EncodeBinary: encodeArrayBinaryValue(elementOID),
		ScanType:     reflect.TypeOf([]driver.Value(nil)),
	})
}

func init() {
	registerArrayCodec(OIDBoolArr, OIDBool, "_bool")
	registerArrayCodec(OIDByteaArr, OIDBytea, "_bytea")
	registerArrayCodec(OIDInt2Arr, OIDInt2, "_int2")
	registerArrayCodec(OIDInt4Arr, OIDInt4, "_int4")
	registerArrayCodec(OIDTextArr, OIDText, "_text")
	registerArrayCodec(OIDInt8Arr, OIDInt8, "_int8")
	registerArrayCodec(OIDFloat4Arr, OIDFloat4, "_float4")
	registerArrayCodec(OIDFloat8Arr, OIDFloat8, "_float8")
	registerArrayCodec(OIDVarcharArr, OIDVarchar, "_varchar")
	registerArrayCodec(OIDUUIDArr, OIDUUID, "_uuid")
}
