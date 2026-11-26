package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A role or database can set DateStyle. The text result format (the simple
// path and the unnamed extended path) then prints dates and timestamps in a
// form the driver does not read. It must be a clear error that names DateStyle,
// never a wrong value.
func TestDecodeTextNonISODateStyleIsAClearError(t *testing.T) {
	for _, c := range []struct {
		oid  uint32
		text string
	}{
		// SQL style (MDY and DMY), Postgres style, German style.
		{protocol.OIDDate, "11/22/2026"},
		{protocol.OIDDate, "22/11/2026"},
		{protocol.OIDDate, "11-22-2026"},
		{protocol.OIDDate, "22.11.2026"},
		{protocol.OIDTimestamp, "11/22/2026 12:34:56"},
		{protocol.OIDTimestamp, "22/11/2026 12:34:56.789012"},
		{protocol.OIDTimestamp, "Sun Nov 22 12:34:56 2026"},
		{protocol.OIDTimestamp, "22.11.2026 12:34:56"},
		{protocol.OIDTimestamptz, "11/22/2026 12:34:56 UTC"},
		{protocol.OIDTimestamptz, "22/11/2026 12:34:56+01"},
		{protocol.OIDTimestamptz, "Sun Nov 22 12:34:56 2026 CET"},
		{protocol.OIDTimestamptz, "22.11.2026 12:34:56 CET"},
	} {
		codec := protocol.LookupOID(c.oid)
		v, err := decodeText(codec, []byte(c.text))
		if err == nil {
			t.Errorf("oid %d %q: got %v, want an error", c.oid, c.text, v)
			continue
		}
		if !strings.Contains(err.Error(), "DateStyle") {
			t.Errorf("oid %d %q: error %q does not name DateStyle", c.oid, c.text, err)
		}
	}
}

func TestDecodeTextISODateStyleStillDecodes(t *testing.T) {
	want := time.Date(2026, 11, 22, 12, 34, 56, 789012000, time.UTC)
	for _, c := range []struct {
		oid  uint32
		text string
		want time.Time
	}{
		{protocol.OIDDate, "2026-11-22", time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)},
		{protocol.OIDTimestamp, "2026-11-22 12:34:56.789012", want},
		{protocol.OIDTimestamptz, "2026-11-22 12:34:56.789012+00", want},
		{protocol.OIDTimestamptz, "2026-11-22 18:04:56.789012+05:30", want},
	} {
		v, err := decodeText(protocol.LookupOID(c.oid), []byte(c.text))
		if err != nil {
			t.Errorf("oid %d %q: %v", c.oid, c.text, err)
			continue
		}
		if tm, ok := v.(time.Time); !ok || !tm.Equal(c.want) {
			t.Errorf("oid %d %q: got %v, want %v", c.oid, c.text, v, c.want)
		}
	}
}

// A codec that is not a date or timestamp keeps its own error unchanged.
func TestDecodeTextOtherErrorsUnchanged(t *testing.T) {
	_, err := decodeText(protocol.LookupOID(protocol.OIDInt4), []byte("zz"))
	if err == nil || strings.Contains(err.Error(), "DateStyle") {
		t.Errorf("int4 text error = %v, want a plain error without a DateStyle hint", err)
	}
}
