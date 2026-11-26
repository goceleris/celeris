package postgres

import (
	"database/sql/driver"
	"fmt"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// decodeText runs codec's text decoder. The driver reads date, timestamp and
// timestamptz text in the ISO DateStyle output only (the PostgreSQL default and
// what a session reports as "ISO, MDY" / "ISO, DMY" / "ISO, YMD"): the SQL,
// Postgres and German styles print a different layout, which the decoders
// reject and never mistake for another date. The rejection names DateStyle, so
// a role or database set to one of those styles gets a clear error, not a wrong
// value. (The binary result format does not depend on DateStyle.)
func decodeText(codec *protocol.TypeCodec, raw []byte) (driver.Value, error) {
	v, err := codec.DecodeText(raw)
	if err != nil {
		switch codec.OID {
		case protocol.OIDDate, protocol.OIDTimestamp, protocol.OIDTimestamptz:
			return nil, fmt.Errorf("%w (celeris-postgres: dates and timestamps in text format are read in the ISO DateStyle only; "+
				"if SHOW DateStyle is not ISO, set it to ISO on the role or database)", err)
		}
	}
	return v, err
}
