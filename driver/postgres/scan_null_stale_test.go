package postgres

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A NULL cell must never leave the destination as it was: a variable reused
// across rows would otherwise carry the previous row's value into a row whose
// column is NULL, with no error. Every destination kind the native Rows.Scan
// accepts is either set to its zero value (nil for *any, a pointer-to-pointer,
// a []byte and a json.RawMessage; Valid=false for the sql.Null* types) or the
// Scan returns an error. No destination is left unchanged.
//
// The table runs both scan paths: scanRaw (the production *pgRows path, a nil
// raw cell) and scanValue (the driver.Value path used by streamed results and
// by Rows over any other driver.Rows).

type nullDest struct {
	name string
	// mk returns a pointer pre-filled with a non-zero sentinel and a function
	// reporting whether the destination now holds its NULL state.
	mk func() (ptr any, isNull func() bool)
}

func nd[T any](name string, sentinel T, isNull func(T) bool) nullDest {
	return nullDest{name, func() (any, func() bool) {
		v := sentinel
		return &v, func() bool { return isNull(v) }
	}}
}

func ndPtr[T any](name string, sentinel T) nullDest {
	return nullDest{name, func() (any, func() bool) {
		v := new(T)
		*v = sentinel
		return &v, func() bool { return v == nil }
	}}
}

func isZero[T comparable](v T) bool { var z T; return v == z }

func nullDests() []nullDest {
	ts := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	var anyS any = "STALE"
	return []nullDest{
		nd("*string", "STALE", isZero[string]),
		nd("*[]byte", []byte("STALE"), func(v []byte) bool { return v == nil }),
		nd("*json.RawMessage", json.RawMessage("STALE"), func(v json.RawMessage) bool { return v == nil }),
		nd("*int", 7, isZero[int]),
		nd("*int16", int16(7), isZero[int16]),
		nd("*int32", int32(7), isZero[int32]),
		nd("*int64", int64(7), isZero[int64]),
		nd("*uint32", uint32(7), isZero[uint32]),
		nd("*float32", float32(7), isZero[float32]),
		nd("*float64", float64(7), isZero[float64]),
		nd("*bool", true, isZero[bool]),
		nd("*time.Time", ts, func(v time.Time) bool { return v.IsZero() }),
		nd("*any", anyS, func(v any) bool { return v == nil }),
		nd("*sql.NullString", sql.NullString{String: "STALE", Valid: true}, func(v sql.NullString) bool { return v == sql.NullString{} }),
		nd("*sql.NullInt64", sql.NullInt64{Int64: 7, Valid: true}, func(v sql.NullInt64) bool { return v == sql.NullInt64{} }),
		nd("*sql.NullFloat64", sql.NullFloat64{Float64: 7, Valid: true}, func(v sql.NullFloat64) bool { return v == sql.NullFloat64{} }),
		nd("*sql.NullBool", sql.NullBool{Bool: true, Valid: true}, func(v sql.NullBool) bool { return v == sql.NullBool{} }),
		nd("*sql.NullTime", sql.NullTime{Time: ts, Valid: true}, func(v sql.NullTime) bool { return !v.Valid }),
		ndPtr("**string", "STALE"),
		ndPtr("**int64", int64(7)),
		ndPtr("**float64", float64(7)),
		ndPtr("**bool", true),
		ndPtr("**time.Time", ts),
		ndPtr("**[]byte", []byte("STALE")),
	}
}

// Destinations Scan does not support for any value. A NULL into one of them
// must fail the same way a non-NULL value does, not succeed and leave it be.
func unsupportedNullDests() map[string]any {
	return map[string]any{
		"*uint64":       new(uint64),
		"*int8":         new(int8),
		"*[]string":     new([]string),
		"*struct{}":     new(struct{}),
		"string":        "not a pointer",
		"*sql.RawBytes": new(sql.RawBytes),
	}
}

func TestScanNullNeverLeavesStaleValue(t *testing.T) {
	// A typed codec per OID the way Rows hands it to scanRaw; the nil cell is
	// handled before the codec is consulted, so the type must not matter.
	oids := []uint32{
		protocol.OIDInt2, protocol.OIDInt4, protocol.OIDInt8, protocol.OIDFloat4, protocol.OIDFloat8,
		protocol.OIDNumeric, protocol.OIDText, protocol.OIDBool, protocol.OIDBytea, protocol.OIDUUID,
		protocol.OIDDate, protocol.OIDTimestamp, protocol.OIDTimestamptz, protocol.OIDJSON, protocol.OIDJSONB,
	}
	for _, d := range nullDests() {
		t.Run(d.name, func(t *testing.T) {
			stale := false
			for _, oid := range oids {
				if stale {
					break
				}
				codec := protocol.LookupOID(oid)
				for _, textFormat := range []bool{false, true} {
					col := protocol.ColumnDesc{TypeOID: oid, FormatCode: protocol.FormatBinary}
					ptr, isNull := d.mk()
					if err := scanRaw(ptr, nil, codec, col, textFormat); err != nil {
						t.Fatalf("scanRaw(NULL, oid %d): unexpected error %v", oid, err)
					}
					if !isNull() {
						stale = true
						t.Errorf("scanRaw(NULL, oid %d, text=%v): destination keeps its previous value: %#v", oid, textFormat, reflect.ValueOf(ptr).Elem().Interface())
					}
				}
			}
			ptr, isNull := d.mk()
			if err := scanValue(ptr, nil); err != nil {
				t.Fatalf("scanValue(nil): unexpected error %v", err)
			}
			if !isNull() {
				t.Errorf("scanValue(nil): destination keeps its previous value: %#v", reflect.ValueOf(ptr).Elem().Interface())
			}
		})
	}
}

func TestScanNullUnsupportedDestErrors(t *testing.T) {
	codec := protocol.LookupOID(protocol.OIDInt4)
	col := protocol.ColumnDesc{TypeOID: protocol.OIDInt4, FormatCode: protocol.FormatBinary}
	for name, dest := range unsupportedNullDests() {
		t.Run(name, func(t *testing.T) {
			// A non-NULL value fails; so must NULL.
			if err := scanValue(dest, int64(1)); err == nil {
				t.Fatalf("scanValue(1) into %s: want an error, got nil", name)
			}
			if err := scanRaw(dest, nil, codec, col, false); err == nil || !strings.Contains(err.Error(), "unsupported scan dest type") {
				t.Errorf("scanRaw(NULL) into %s: err = %v, want unsupported scan dest type", name, err)
			}
			if err := scanValue(dest, nil); err == nil || !strings.Contains(err.Error(), "unsupported scan dest type") {
				t.Errorf("scanValue(nil) into %s: err = %v, want unsupported scan dest type", name, err)
			}
		})
	}
}

// A pointer-to-pointer is allocated for a value and set to nil for NULL, as
// database/sql and pgx do, so a nullable column scans into *T.
func TestScanPointerToPointer(t *testing.T) {
	var p *int64
	if err := scanValue(&p, int64(42)); err != nil || p == nil || *p != 42 {
		t.Fatalf("**int64 <- 42: %v, err %v", p, err)
	}
	if err := scanValue(&p, nil); err != nil || p != nil {
		t.Fatalf("**int64 <- NULL: %v, err %v, want nil", p, err)
	}
	var s *string
	if err := scanValue(&s, "x"); err != nil || s == nil || *s != "x" {
		t.Fatalf("**string <- x: %v, err %v", s, err)
	}
	var ns *sql.NullString
	if err := scanValue(&ns, "y"); err != nil || ns == nil || !ns.Valid || ns.String != "y" {
		t.Fatalf("**sql.NullString <- y: %+v, err %v", ns, err)
	}
}
