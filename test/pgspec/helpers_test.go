//go:build pgspec

package pgspec

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// --- Expect helpers: each reads one message and validates its type ---

// ExpectAuth reads one message and asserts it is an Authentication message.
// Returns the auth subtype int32.
func (c *pgRawConn) ExpectAuth(t *testing.T) int32 {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendAuthentication {
		t.Fatalf("expected Authentication ('R'), got %q", msgType)
	}
	if len(payload) < 4 {
		t.Fatal("short Authentication payload")
	}
	return int32(binary.BigEndian.Uint32(payload[:4]))
}

// ExpectError reads one message and asserts it is an ErrorResponse. Returns
// the parsed PGError.
func (c *pgRawConn) ExpectError(t *testing.T) *protocol.PGError {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendErrorResponse {
		t.Fatalf("expected ErrorResponse ('E'), got %q", msgType)
	}
	return protocol.ParseErrorResponse(payload)
}

// ExpectRowDescription reads one message, asserts it is a RowDescription, and
// returns the column descriptors.
func (c *pgRawConn) ExpectRowDescription(t *testing.T) []protocol.ColumnDesc {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendRowDescription {
		t.Fatalf("expected RowDescription ('T'), got %q", msgType)
	}
	cols, err := protocol.ParseRowDescription(payload)
	if err != nil {
		t.Fatalf("ParseRowDescription: %v", err)
	}
	return cols
}

// ExpectDataRow reads one message, asserts it is a DataRow, and returns the
// field values.
func (c *pgRawConn) ExpectDataRow(t *testing.T) [][]byte {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendDataRow {
		t.Fatalf("expected DataRow ('D'), got %q", msgType)
	}
	fields, err := protocol.ParseDataRow(payload)
	if err != nil {
		t.Fatalf("ParseDataRow: %v", err)
	}
	return fields
}

// ExpectCommandComplete reads one message, asserts it is a CommandComplete,
// and returns the tag string.
func (c *pgRawConn) ExpectCommandComplete(t *testing.T) string {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendCommandComplete {
		t.Fatalf("expected CommandComplete ('C'), got %q", msgType)
	}
	tag, err := protocol.ParseCommandComplete(payload)
	if err != nil {
		t.Fatalf("ParseCommandComplete: %v", err)
	}
	return tag
}

// ExpectReadyForQuery reads one message, asserts it is ReadyForQuery, and
// returns the transaction status byte ('I', 'T', or 'E').
func (c *pgRawConn) ExpectReadyForQuery(t *testing.T) byte {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendReadyForQuery {
		t.Fatalf("expected ReadyForQuery ('Z'), got %q", msgType)
	}
	if len(payload) < 1 {
		t.Fatal("short ReadyForQuery payload")
	}
	return payload[0]
}

// ExpectParseComplete reads one message and asserts it is ParseComplete.
func (c *pgRawConn) ExpectParseComplete(t *testing.T) {
	t.Helper()
	msgType, _ := c.ReadMsg(t)
	if msgType != protocol.BackendParseComplete {
		t.Fatalf("expected ParseComplete ('1'), got %q", msgType)
	}
}

// ExpectBindComplete reads one message and asserts it is BindComplete.
func (c *pgRawConn) ExpectBindComplete(t *testing.T) {
	t.Helper()
	msgType, _ := c.ReadMsg(t)
	if msgType != protocol.BackendBindComplete {
		t.Fatalf("expected BindComplete ('2'), got %q", msgType)
	}
}

// ExpectCloseComplete reads one message and asserts it is CloseComplete.
func (c *pgRawConn) ExpectCloseComplete(t *testing.T) {
	t.Helper()
	msgType, _ := c.ReadMsg(t)
	if msgType != protocol.BackendCloseComplete {
		t.Fatalf("expected CloseComplete ('3'), got %q", msgType)
	}
}

// ExpectNoData reads one message and asserts it is NoData.
func (c *pgRawConn) ExpectNoData(t *testing.T) {
	t.Helper()
	msgType, _ := c.ReadMsg(t)
	if msgType != protocol.BackendNoData {
		t.Fatalf("expected NoData ('n'), got %q", msgType)
	}
}

// ExpectEmptyQuery reads one message and asserts it is EmptyQueryResponse.
func (c *pgRawConn) ExpectEmptyQuery(t *testing.T) {
	t.Helper()
	msgType, _ := c.ReadMsg(t)
	if msgType != protocol.BackendEmptyQuery {
		t.Fatalf("expected EmptyQueryResponse ('I'), got %q", msgType)
	}
}

// ExpectPortalSuspended reads one message and asserts it is PortalSuspended.
func (c *pgRawConn) ExpectPortalSuspended(t *testing.T) {
	t.Helper()
	msgType, _ := c.ReadMsg(t)
	if msgType != protocol.BackendPortalSuspended {
		t.Fatalf("expected PortalSuspended ('s'), got %q", msgType)
	}
}

// ExpectParameterDescription reads one message, asserts it is
// ParameterDescription, and returns the param OIDs.
func (c *pgRawConn) ExpectParameterDescription(t *testing.T) []uint32 {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendParameterDesc {
		t.Fatalf("expected ParameterDescription ('t'), got %q", msgType)
	}
	oids, err := protocol.ParseParameterDescription(payload)
	if err != nil {
		t.Fatalf("ParseParameterDescription: %v", err)
	}
	return oids
}

// ExpectNotice reads one message and asserts it is a NoticeResponse.
func (c *pgRawConn) ExpectNotice(t *testing.T) *protocol.PGError {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendNoticeResponse {
		t.Fatalf("expected NoticeResponse ('N'), got %q", msgType)
	}
	return protocol.ParseErrorResponse(payload)
}

// ExpectCopyInResponse reads one message and asserts it is CopyInResponse.
func (c *pgRawConn) ExpectCopyInResponse(t *testing.T) protocol.CopyResponse {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendCopyInResponse {
		t.Fatalf("expected CopyInResponse ('G'), got %q", msgType)
	}
	resp, err := protocol.ParseCopyResponse(payload)
	if err != nil {
		t.Fatalf("ParseCopyResponse: %v", err)
	}
	return resp
}

// ExpectCopyOutResponse reads one message and asserts it is CopyOutResponse.
func (c *pgRawConn) ExpectCopyOutResponse(t *testing.T) protocol.CopyResponse {
	t.Helper()
	msgType, payload := c.ReadMsg(t)
	if msgType != protocol.BackendCopyOutResponse {
		t.Fatalf("expected CopyOutResponse ('H'), got %q", msgType)
	}
	resp, err := protocol.ParseCopyResponse(payload)
	if err != nil {
		t.Fatalf("ParseCopyResponse: %v", err)
	}
	return resp
}

// --- Message builder helpers ---

// buildQuery returns a Query ('Q') message as bytes.
func buildQuery(w *protocol.Writer, sql string) []byte {
	return protocol.WriteQuery(w, sql)
}

// buildParse returns a Parse ('P') message as bytes.
func buildParse(w *protocol.Writer, name, query string, paramOIDs []uint32) []byte {
	return protocol.WriteParse(w, name, query, paramOIDs)
}

// buildBind returns a Bind ('B') message as bytes.
func buildBind(w *protocol.Writer, portal, stmt string, paramFormats []int16, paramValues [][]byte, resultFormats []int16) []byte {
	return protocol.WriteBind(w, portal, stmt, paramFormats, paramValues, resultFormats)
}

// buildDescribe returns a Describe ('D') message as bytes.
func buildDescribe(w *protocol.Writer, kind byte, name string) []byte {
	return protocol.WriteDescribe(w, kind, name)
}

// buildExecute returns an Execute ('E') message as bytes.
func buildExecute(w *protocol.Writer, portal string, maxRows int32) []byte {
	return protocol.WriteExecute(w, portal, maxRows)
}

// buildSync returns a Sync ('S') message as bytes.
func buildSync(w *protocol.Writer) []byte {
	return protocol.WriteSync(w)
}

// buildClose returns a Close ('C') message as bytes.
func buildClose(w *protocol.Writer, kind byte, name string) []byte {
	return protocol.WriteClose(w, kind, name)
}

// buildFlush returns a Flush ('H') message as bytes.
func buildFlush(w *protocol.Writer) []byte {
	return protocol.WriteFlush(w)
}

// buildCopyData returns a CopyData ('d') message as bytes.
func buildCopyData(w *protocol.Writer, data []byte) []byte {
	return protocol.WriteCopyData(w, data)
}

// buildCopyDone returns a CopyDone ('c') message as bytes.
func buildCopyDone(w *protocol.Writer) []byte {
	return protocol.WriteCopyDone(w)
}

// buildCopyFail returns a CopyFail ('f') message as bytes.
func buildCopyFail(w *protocol.Writer, reason string) []byte {
	return protocol.WriteCopyFail(w, reason)
}

// --- Utility helpers ---

// drainUntilRFQ reads and discards messages until ReadyForQuery. Returns the
// transaction status byte. Useful for skipping over ErrorResponse + RFQ
// sequences where we don't care about the intermediate messages.
func (c *pgRawConn) drainUntilRFQ(t *testing.T) byte {
	t.Helper()
	for {
		msgType, payload := c.ReadMsg(t)
		if msgType == protocol.BackendReadyForQuery {
			if len(payload) < 1 {
				t.Fatal("short ReadyForQuery")
			}
			return payload[0]
		}
	}
}

// drainNoticesAndParams reads and discards ParameterStatus and NoticeResponse
// messages, stopping at (and returning) the first non-notice/non-param
// message.
func (c *pgRawConn) drainNoticesAndParams(t *testing.T) (byte, []byte) {
	t.Helper()
	for {
		msgType, payload := c.ReadMsg(t)
		switch msgType {
		case protocol.BackendParameterStatus, protocol.BackendNoticeResponse:
			continue
		default:
			return msgType, payload
		}
	}
}

// findMessages returns all messages of the given type from a message slice.
func findMessages(msgs []pgMessage, msgType byte) []pgMessage {
	var out []pgMessage
	for _, m := range msgs {
		if m.Type == msgType {
			out = append(out, m)
		}
	}
	return out
}

// hasMessage returns true if any message in the slice has the given type.
func hasMessage(msgs []pgMessage, msgType byte) bool {
	for _, m := range msgs {
		if m.Type == msgType {
			return true
		}
	}
	return false
}

// createTempTable creates a temporary table and returns its name. The table
// is automatically dropped when the connection closes.
func createTempTable(t *testing.T, c *pgRawConn, name, cols string) {
	t.Helper()
	sql := fmt.Sprintf("CREATE TEMP TABLE %s (%s)", name, cols)
	c.SendQuery(sql)
	msgs := c.ReadUntilRFQ(t)
	if hasMessage(msgs, protocol.BackendErrorResponse) {
		for _, m := range msgs {
			if m.Type == protocol.BackendErrorResponse {
				pgErr := protocol.ParseErrorResponse(m.Payload)
				t.Fatalf("CREATE TEMP TABLE: %v", pgErr)
			}
		}
	}
}

// execSimple executes a simple query and returns all messages up to RFQ.
func execSimple(t *testing.T, c *pgRawConn, sql string) []pgMessage {
	t.Helper()
	c.SendQuery(sql)
	return c.ReadUntilRFQ(t)
}

// assertTagPrefix asserts that the CommandComplete tag starts with the given
// prefix (e.g. "SELECT", "INSERT 0").
func assertTagPrefix(t *testing.T, tag, prefix string) {
	t.Helper()
	if !strings.HasPrefix(tag, prefix) {
		t.Errorf("tag %q does not start with %q", tag, prefix)
	}
}
