package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// celeris#1104. The sync fast path trips the buffer cap during WriteAndPoll;
// the poll then returns before the reply is done (EAGAIN: a reply still
// arriving is the normal case for a large one), the caller falls through to the
// async wait with syncMode cleared, and the rest of the reply is dispatched by
// the event loop. Rows after the cap must not be buffered then: they would be
// promoted to streaming behind the rows already buffered, with the dropped ones
// missing in between and the error lost (a hole and io.EOF).

const fallThroughRowSize = 64 << 10 // 1024 of them are the 64 MiB cap

func fallThroughRow(i int) []byte {
	p := binary.BigEndian.AppendUint16(nil, 1)
	p = binary.BigEndian.AppendUint32(p, fallThroughRowSize)
	val := make([]byte, fallThroughRowSize)
	binary.BigEndian.PutUint64(val, uint64(i))
	return pgMsg(protocol.BackendDataRow, append(p, val...))
}

// partialSync delivers first, then reports the poll over although the reply is
// not (data came, then EAGAIN).
type partialSync struct{ first []byte }

func (s partialSync) WriteAndPoll(_ int, _ []byte, _ []byte, onRecv func([]byte)) (bool, error) {
	onRecv(s.first)
	return true, nil
}

func TestSyncFallThroughAfterTheCapKeepsOrderAndTheError1104(t *testing.T) {
	const phase1, total = 1100, 1200 // the poll takes 1100 rows, the cap is 1024
	for _, tc := range []struct {
		name   string
		prefix []byte // what the query's reply starts with
		run    func(c *pgConn, ctx context.Context) (driver.Rows, error)
	}{
		{"simpleQuery", pgRowDesc1(), func(c *pgConn, ctx context.Context) (driver.Rows, error) {
			return c.simpleQuery(ctx, "SELECT a FROM t")
		}},
		{"doExtendedQuery", append(pgMsg(protocol.BackendParseComplete, nil), pgMsg(protocol.BackendBindComplete, nil)...), func(c *pgConn, ctx context.Context) (driver.Rows, error) {
			cols := []protocol.ColumnDesc{{Name: "a", TypeOID: protocol.OIDText, FormatCode: protocol.FormatText}}
			return c.doExtendedQuery(ctx, "", "SELECT a FROM t", nil, cols)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := append([]byte(nil), tc.prefix...)
			for i := 0; i < phase1; i++ {
				first = append(first, fallThroughRow(i)...)
			}
			var rest []byte
			for i := phase1; i < total; i++ {
				rest = append(rest, fallThroughRow(i)...)
			}
			rest = append(rest, pgCompleteAndReady()...)
			c := newSyncFaultConn(partialSync{first: first})
			go func() {
				// The event loop dispatches the rest once the caller has fallen
				// through: its request has left the sync mode (and the poll's
				// onRecv has returned, the reader is single-threaded).
				for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); time.Sleep(time.Millisecond) {
					c.pendingMu.Lock()
					fell := len(c.pending) > 0 && c.pending[0] != nil && !c.pending[0].syncMode.Load()
					c.pendingMu.Unlock()
					if fell {
						break
					}
				}
				c.onRecv(rest)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			rows, err := tc.run(c, ctx)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			dest := make([]driver.Value, 1)
			got, holes, prev := 0, 0, -1
			var rerr error
			for {
				if rerr = rows.Next(dest); rerr != nil {
					break
				}
				var b []byte
				switch v := dest[0].(type) {
				case []byte:
					b = v
				case string:
					b = []byte(v)
				}
				if idx := int(binary.BigEndian.Uint64(b[:8])); idx != prev+1 {
					holes++
				} else {
					prev = idx
				}
				got++
			}
			_ = rows.Close()
			if holes != 0 {
				t.Errorf("the result has a gap, and %d rows after it (%d rows read, last index %d)", holes, got, prev)
			}
			if !errors.Is(rerr, ErrResultTooBig) {
				t.Errorf("after %d rows the error is %v (io.EOF = %v), want ErrResultTooBig", got, rerr, rerr == io.EOF)
			}
			if limit := maxDirectResultBytes / fallThroughRowSize; got != limit {
				t.Errorf("read %d rows, want %d (the rows that fit the cap)", got, limit)
			}
		})
	}
}
