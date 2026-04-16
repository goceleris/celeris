//go:build pgspec

package pgspec

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// ============================================================================
// Section 1: Startup (protocol-flow.html, 55.2.1)
// ============================================================================

func TestStartup(t *testing.T) {
	t.Run("Version30", func(t *testing.T) {
		c := dialRaw(t)
		if err := c.sendStartupV3(c.info.User, c.info.Database, nil); err != nil {
			t.Fatalf("send startup: %v", err)
		}
		// First response should be an Authentication message.
		msgType, payload := c.ReadMsg(t)
		if msgType != protocol.BackendAuthentication {
			t.Fatalf("expected Authentication ('R'), got %q", msgType)
		}
		if len(payload) < 4 {
			t.Fatal("short Authentication payload")
		}
		subtype := int32(binary.BigEndian.Uint32(payload[:4]))
		// Accept any valid auth request: Ok, CleartextPassword, MD5, SASL.
		validAuths := map[int32]bool{
			protocol.AuthOK:                true,
			protocol.AuthCleartextPassword: true,
			protocol.AuthMD5Password:       true,
			protocol.AuthSASL:              true,
		}
		if !validAuths[subtype] {
			t.Fatalf("unexpected auth subtype %d", subtype)
		}
	})

	t.Run("WrongVersion", func(t *testing.T) {
		c := dialRaw(t)
		if err := c.sendStartupV2(); err != nil {
			t.Fatalf("send v2 startup: %v", err)
		}
		// Server should respond with ErrorResponse or close the connection.
		c.SetDeadline(5 * time.Second)
		msgType, payload, err := c.readMsg()
		if err != nil {
			// Connection closed is acceptable.
			return
		}
		if msgType == protocol.BackendErrorResponse {
			pgErr := protocol.ParseErrorResponse(payload)
			t.Logf("server rejected v2.0 startup: %v", pgErr)
			return
		}
		t.Fatalf("expected ErrorResponse or connection close for v2.0, got %q", msgType)
	})

	t.Run("UnknownParam", func(t *testing.T) {
		c := dialRaw(t)
		params := map[string]string{
			"x_celeris_unknown_pgspec_param": "test_value",
		}
		if err := c.sendStartupV3(c.info.User, c.info.Database, params); err != nil {
			t.Fatalf("send startup: %v", err)
		}
		// PG may accept unknown params gracefully or reject them with a
		// FATAL ErrorResponse (PG 16 sends FATAL 42704). Both are valid.
		state := &protocol.StartupState{
			User:     c.info.User,
			Password: c.info.Password,
			Database: c.info.Database,
			Params:   params,
		}
		state.Start(c.writer) // advance state to phaseAwaitAuth

		for {
			msgType, payload := c.ReadMsg(t)
			if msgType == protocol.BackendErrorResponse {
				pgErr := protocol.ParseErrorResponse(payload)
				t.Logf("server rejected unknown param (valid PG 16 behavior): %v", pgErr)
				return
			}
			resp, done, err := state.Handle(msgType, payload, c.writer)
			if err != nil {
				t.Fatalf("startup handle: %v", err)
			}
			if resp != nil {
				if err := c.Send(resp); err != nil {
					t.Fatalf("send auth response: %v", err)
				}
			}
			if done {
				// Startup completed despite unknown param.
				return
			}
		}
	})

	t.Run("SSLRequest", func(t *testing.T) {
		c := dialRaw(t)
		if err := c.sendSSLRequest(); err != nil {
			t.Fatalf("send SSLRequest: %v", err)
		}
		// Server responds with a single byte: 'N' (no SSL) or 'S' (SSL).
		b := c.readOneByte(t)
		if b != 'N' && b != 'S' {
			t.Fatalf("SSLRequest response: got %q, want 'N' or 'S'", b)
		}
		t.Logf("SSLRequest response: %q", b)
	})

	t.Run("CancelRequest", func(t *testing.T) {
		// First establish a real connection to get PID/Secret.
		main := dialPG(t)
		pid := main.PID
		secret := main.Secret

		// Open a second raw connection and send a CancelRequest.
		cancel := dialRaw(t)
		if err := cancel.sendCancelRequest(pid, secret); err != nil {
			t.Fatalf("send CancelRequest: %v", err)
		}
		// Server should close the cancel connection without sending a response.
		cancel.expectConnClosed(t)
	})

	t.Run("GarbageFirst", func(t *testing.T) {
		c := dialRaw(t)
		garbage := make([]byte, 64)
		_, _ = rand.Read(garbage)
		if err := c.Send(garbage); err != nil {
			t.Fatalf("send garbage: %v", err)
		}
		// Server should disconnect.
		c.SetDeadline(5 * time.Second)
		_, _, err := c.readMsg()
		if err == nil {
			// If we got a message, it should be an error.
			return
		}
		// Connection closed is the expected outcome.
	})

	t.Run("TruncatedStartup", func(t *testing.T) {
		c := dialRaw(t)
		// Build a valid startup message.
		c.writer.Reset()
		c.writer.StartStartupMessage()
		c.writer.WriteInt32(protocol.ProtocolVersion)
		c.writer.WriteString("user")
		c.writer.WriteString(c.info.User)
		_ = c.writer.WriteByte(0)
		c.writer.FinishMessage()
		full := c.writer.Bytes()

		// Send only half.
		half := make([]byte, len(full)/2)
		copy(half, full[:len(full)/2])
		if err := c.Send(half); err != nil {
			t.Fatalf("send truncated: %v", err)
		}
		// Close our write side to signal EOF.
		if tc, ok := c.conn.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}

		c.SetDeadline(5 * time.Second)
		_, _, err := c.readMsg()
		if err == nil {
			// Server might send an error first.
			return
		}
		// Timeout or connection reset is acceptable.
	})
}

// ============================================================================
// Section 2: Authentication (55.2.2)
// ============================================================================

func TestAuth(t *testing.T) {
	t.Run("FullHandshake", func(t *testing.T) {
		// Uses the StartupState machine to perform whatever auth the server
		// requires (trust, password, MD5, SCRAM).
		c := dialPG(t)
		// If we get here, auth succeeded. Verify with a simple query.
		c.SendQuery("SELECT 1")
		status := c.drainUntilRFQ(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})

	t.Run("SCRAM_BadPassword", func(t *testing.T) {
		c := dialRaw(t)
		state := &protocol.StartupState{
			User:     c.info.User,
			Password: "definitely_wrong_password_pgspec",
			Database: c.info.Database,
		}
		startBytes := state.Start(c.writer)
		if err := c.Send(startBytes); err != nil {
			t.Fatalf("send startup: %v", err)
		}

		for {
			msgType, payload, err := c.readMsg()
			if err != nil {
				// Connection closed is acceptable for auth failure.
				return
			}
			if msgType == protocol.BackendErrorResponse {
				pgErr := protocol.ParseErrorResponse(payload)
				t.Logf("auth error (expected): %v", pgErr)
				if pgErr.Code != "28P01" && pgErr.Code != "28000" {
					t.Logf("unexpected SQLSTATE %s (expected 28P01 or 28000, but some auth methods differ)", pgErr.Code)
				}
				return
			}
			resp, done, err := state.Handle(msgType, payload, c.writer)
			if err != nil {
				// Auth failure from the protocol layer (e.g. SCRAM mismatch).
				t.Logf("auth state machine error (expected): %v", err)
				return
			}
			if resp != nil {
				if err := c.Send(resp); err != nil {
					return
				}
			}
			if done {
				t.Fatal("auth succeeded with wrong password")
			}
		}
	})
}

// ============================================================================
// Section 3: Simple Query (55.2.3)
// ============================================================================

func TestSimpleQuery(t *testing.T) {
	t.Run("Select", func(t *testing.T) {
		c := dialPG(t)

		c.SendQuery("SELECT 1 AS num")

		cols := c.ExpectRowDescription(t)
		if len(cols) != 1 {
			t.Fatalf("expected 1 column, got %d", len(cols))
		}
		if cols[0].Name != "num" {
			t.Errorf("column name: got %q, want %q", cols[0].Name, "num")
		}

		fields := c.ExpectDataRow(t)
		if len(fields) != 1 {
			t.Fatalf("expected 1 field, got %d", len(fields))
		}
		if string(fields[0]) != "1" {
			t.Errorf("field value: got %q, want %q", fields[0], "1")
		}

		tag := c.ExpectCommandComplete(t)
		assertTagPrefix(t, tag, "SELECT 1")

		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})

	t.Run("Insert", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_insert", "id serial primary key, val text")

		c.SendQuery("INSERT INTO pgspec_insert(val) VALUES ('hello')")
		tag := c.ExpectCommandComplete(t)
		assertTagPrefix(t, tag, "INSERT 0 1")

		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})

	t.Run("Error", func(t *testing.T) {
		c := dialPG(t)

		c.SendQuery("SELECT FROM nonexistent_pgspec_table_xyz")
		msgs := c.ReadUntilRFQ(t)

		if !hasMessage(msgs, protocol.BackendErrorResponse) {
			t.Error("expected ErrorResponse for bad query")
		}
		// Must still end with ReadyForQuery.
		last := msgs[len(msgs)-1]
		if last.Type != protocol.BackendReadyForQuery {
			t.Errorf("last message: got %q, want ReadyForQuery", last.Type)
		}
	})

	t.Run("MultiStatement", func(t *testing.T) {
		c := dialPG(t)

		c.SendQuery("SELECT 1 AS a; SELECT 2 AS b")

		// First result set.
		cols1 := c.ExpectRowDescription(t)
		if len(cols1) != 1 || cols1[0].Name != "a" {
			t.Errorf("first result: unexpected column %v", cols1)
		}
		fields1 := c.ExpectDataRow(t)
		if string(fields1[0]) != "1" {
			t.Errorf("first result value: got %q", fields1[0])
		}
		tag1 := c.ExpectCommandComplete(t)
		assertTagPrefix(t, tag1, "SELECT 1")

		// Second result set.
		cols2 := c.ExpectRowDescription(t)
		if len(cols2) != 1 || cols2[0].Name != "b" {
			t.Errorf("second result: unexpected column %v", cols2)
		}
		fields2 := c.ExpectDataRow(t)
		if string(fields2[0]) != "2" {
			t.Errorf("second result value: got %q", fields2[0])
		}
		tag2 := c.ExpectCommandComplete(t)
		assertTagPrefix(t, tag2, "SELECT 1")

		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})

	t.Run("EmptyQuery", func(t *testing.T) {
		c := dialPG(t)

		c.SendQuery("")

		c.ExpectEmptyQuery(t)

		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})

	t.Run("NULL", func(t *testing.T) {
		c := dialPG(t)

		c.SendQuery("SELECT NULL AS n")

		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if len(fields) != 1 {
			t.Fatalf("expected 1 field, got %d", len(fields))
		}
		if fields[0] != nil {
			t.Errorf("expected NULL (nil), got %q", fields[0])
		}

		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("LargeResult", func(t *testing.T) {
		c := dialPG(t)
		c.SetDeadline(60 * time.Second)

		c.SendQuery("SELECT generate_series(1, 100000)::text AS n")

		_ = c.ExpectRowDescription(t)

		rowCount := 0
		for {
			msgType, payload := c.ReadMsg(t)
			switch msgType {
			case protocol.BackendDataRow:
				rowCount++
				fields, err := protocol.ParseDataRow(payload)
				if err != nil {
					t.Fatalf("row %d: ParseDataRow: %v", rowCount, err)
				}
				expected := strconv.Itoa(rowCount)
				if string(fields[0]) != expected {
					t.Fatalf("row %d: got %q, want %q", rowCount, fields[0], expected)
				}
			case protocol.BackendCommandComplete:
				tag, _ := protocol.ParseCommandComplete(payload)
				assertTagPrefix(t, tag, "SELECT 100000")
				goto rfq
			default:
				t.Fatalf("unexpected message %q at row %d", msgType, rowCount)
			}
		}
	rfq:
		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
		if rowCount != 100000 {
			t.Errorf("row count: got %d, want 100000", rowCount)
		}
	})

	t.Run("TransactionStatus", func(t *testing.T) {
		c := dialPG(t)

		// Idle.
		c.SendQuery("SELECT 1")
		msgs := c.ReadUntilRFQ(t)
		last := msgs[len(msgs)-1]
		if last.Payload[0] != 'I' {
			t.Errorf("idle status: got %q, want 'I'", last.Payload[0])
		}

		// In transaction.
		c.SendQuery("BEGIN")
		msgs = c.ReadUntilRFQ(t)
		last = msgs[len(msgs)-1]
		if last.Payload[0] != 'T' {
			t.Errorf("in-tx status: got %q, want 'T'", last.Payload[0])
		}

		// Failed transaction.
		c.SendQuery("SELECT FROM nonexistent_pgspec_table_xyz")
		msgs = c.ReadUntilRFQ(t)
		last = msgs[len(msgs)-1]
		if last.Payload[0] != 'E' {
			t.Errorf("failed-tx status: got %q, want 'E'", last.Payload[0])
		}

		// Rollback to idle.
		c.SendQuery("ROLLBACK")
		msgs = c.ReadUntilRFQ(t)
		last = msgs[len(msgs)-1]
		if last.Payload[0] != 'I' {
			t.Errorf("post-rollback status: got %q, want 'I'", last.Payload[0])
		}
	})
}

// ============================================================================
// Section 4: Extended Query (55.2.4)
// ============================================================================

func TestExtended(t *testing.T) {
	t.Run("ParseBindExecute", func(t *testing.T) {
		c := dialPG(t)

		w := c.writer
		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECT $1::int AS val", nil)...)
		buf = append(buf, buildBind(w, "", "", []int16{0}, [][]byte{[]byte("42")}, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send extended: %v", err)
		}

		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)

		// In the extended query protocol, Execute does not produce a
		// RowDescription -- that only comes from Describe.
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "42" {
			t.Errorf("value: got %q, want %q", fields[0], "42")
		}
		c.ExpectCommandComplete(t)
		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})

	t.Run("NamedStatement", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Parse with a name.
		var buf []byte
		buf = append(buf, buildParse(w, "pgspec_named", "SELECT $1::text AS echo", nil)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send parse: %v", err)
		}
		c.ExpectParseComplete(t)
		c.ExpectReadyForQuery(t)

		// Reuse across multiple Bind+Execute.
		for i := range 3 {
			val := fmt.Sprintf("iter_%d", i)
			buf = buf[:0]
			buf = append(buf, buildBind(w, "", "pgspec_named", []int16{0}, [][]byte{[]byte(val)}, nil)...)
			buf = append(buf, buildExecute(w, "", 0)...)
			buf = append(buf, buildSync(w)...)
			if err := c.Send(buf); err != nil {
				t.Fatalf("send bind/execute iter %d: %v", i, err)
			}
			c.ExpectBindComplete(t)
			fields := c.ExpectDataRow(t)
			if string(fields[0]) != val {
				t.Errorf("iter %d: got %q, want %q", i, fields[0], val)
			}
			c.ExpectCommandComplete(t)
			c.ExpectReadyForQuery(t)
		}

		// Clean up: close the named statement.
		buf = buf[:0]
		buf = append(buf, buildClose(w, 'S', "pgspec_named")...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send close: %v", err)
		}
		c.ExpectCloseComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("Describe", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		var buf []byte
		buf = append(buf, buildParse(w, "pgspec_desc", "SELECT $1::int AS val, $2::text AS txt", nil)...)
		buf = append(buf, buildDescribe(w, 'S', "pgspec_desc")...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		c.ExpectParseComplete(t)
		paramOIDs := c.ExpectParameterDescription(t)
		if len(paramOIDs) != 2 {
			t.Fatalf("expected 2 params, got %d", len(paramOIDs))
		}
		cols := c.ExpectRowDescription(t)
		if len(cols) != 2 {
			t.Fatalf("expected 2 columns, got %d", len(cols))
		}
		if cols[0].Name != "val" || cols[1].Name != "txt" {
			t.Errorf("column names: got [%q, %q]", cols[0].Name, cols[1].Name)
		}
		c.ExpectReadyForQuery(t)

		// Clean up.
		buf = buf[:0]
		buf = append(buf, buildClose(w, 'S', "pgspec_desc")...)
		buf = append(buf, buildSync(w)...)
		_ = c.Send(buf)
		c.ExpectCloseComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("DescribePortal", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECT 1 AS x", nil)...)
		buf = append(buf, buildBind(w, "pgspec_portal", "", nil, nil, nil)...)
		buf = append(buf, buildDescribe(w, 'P', "pgspec_portal")...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		cols := c.ExpectRowDescription(t)
		if len(cols) != 1 || cols[0].Name != "x" {
			t.Errorf("portal describe: unexpected columns %v", cols)
		}
		c.ExpectReadyForQuery(t)
	})

	t.Run("NoData", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_nodata", "id int")
		w := c.writer

		var buf []byte
		buf = append(buf, buildParse(w, "", "INSERT INTO pgspec_nodata VALUES ($1::int)", nil)...)
		buf = append(buf, buildDescribe(w, 'S', "")...)
		buf = append(buf, buildBind(w, "", "", []int16{0}, [][]byte{[]byte("1")}, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		c.ExpectParseComplete(t)
		_ = c.ExpectParameterDescription(t)
		c.ExpectNoData(t)
		c.ExpectBindComplete(t)
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("ParameterTypes", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Bind with binary parameter format.
		intBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(intBytes, 42)

		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECT $1::int4 AS val", []uint32{protocol.OIDInt4})...)
		buf = append(buf, buildBind(w, "", "", []int16{1}, [][]byte{intBytes}, []int16{0})...) // param binary, result text
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "42" {
			t.Errorf("binary int param: got %q, want %q", fields[0], "42")
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("CloseStatement", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		var buf []byte
		buf = append(buf, buildParse(w, "pgspec_close", "SELECT 1", nil)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send parse: %v", err)
		}
		c.ExpectParseComplete(t)
		c.ExpectReadyForQuery(t)

		buf = buf[:0]
		buf = append(buf, buildClose(w, 'S', "pgspec_close")...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send close: %v", err)
		}
		c.ExpectCloseComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("ErrorDuringExtended", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Parse with syntax error.
		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECTT BOGUS SYNTAX", nil)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		// Expect ErrorResponse, then ReadyForQuery.
		pgErr := c.ExpectError(t)
		if pgErr.Code == "" {
			t.Error("expected SQLSTATE code in error")
		}
		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status after error: got %q, want 'I'", status)
		}

		// Connection should still be usable.
		c.SendQuery("SELECT 1")
		msgs := c.ReadUntilRFQ(t)
		if !hasMessage(msgs, protocol.BackendDataRow) {
			t.Error("connection not usable after extended query error")
		}
	})

	t.Run("PortalSuspended", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Use Flush (not Sync) between Execute calls so the named portal
		// survives. PG destroys portals bound to the unnamed statement
		// when Sync processes, even if the portal itself is named.
		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECT generate_series(1, 5)::text AS n", nil)...)
		buf = append(buf, buildBind(w, "pgspec_susp", "", nil, nil, nil)...)
		buf = append(buf, buildExecute(w, "pgspec_susp", 1)...) // maxRows=1
		buf = append(buf, buildFlush(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "1" {
			t.Errorf("first row: got %q, want %q", fields[0], "1")
		}
		c.ExpectPortalSuspended(t)

		// Fetch more rows from the suspended portal (still using Flush).
		buf = buf[:0]
		buf = append(buf, buildExecute(w, "pgspec_susp", 2)...)
		buf = append(buf, buildFlush(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send execute: %v", err)
		}

		// Should get 2 more DataRows.
		f1 := c.ExpectDataRow(t)
		f2 := c.ExpectDataRow(t)
		if string(f1[0]) != "2" || string(f2[0]) != "3" {
			t.Errorf("continued rows: got [%q, %q], want [2, 3]", f1[0], f2[0])
		}
		c.ExpectPortalSuspended(t)

		// Now Sync to finalize.
		buf = buf[:0]
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send sync: %v", err)
		}
		c.ExpectReadyForQuery(t)
	})

	t.Run("Pipeline", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Send two Parse+Bind+Execute sequences with a single Sync at the end.
		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECT 10 AS a", nil)...)
		buf = append(buf, buildBind(w, "", "", nil, nil, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildParse(w, "", "SELECT 20 AS b", nil)...)
		buf = append(buf, buildBind(w, "", "", nil, nil, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send pipeline: %v", err)
		}

		// First query.
		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		f1 := c.ExpectDataRow(t)
		if string(f1[0]) != "10" {
			t.Errorf("pipeline q1: got %q, want %q", f1[0], "10")
		}
		c.ExpectCommandComplete(t)

		// Second query.
		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		f2 := c.ExpectDataRow(t)
		if string(f2[0]) != "20" {
			t.Errorf("pipeline q2: got %q, want %q", f2[0], "20")
		}
		c.ExpectCommandComplete(t)

		status := c.ExpectReadyForQuery(t)
		if status != 'I' {
			t.Errorf("RFQ status: got %q, want 'I'", status)
		}
	})
}

// ============================================================================
// Section 5: COPY (55.2.5)
// ============================================================================

func TestCopy(t *testing.T) {
	t.Run("TextIn", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_copyin", "id int, name text")
		w := c.writer

		c.SendQuery("COPY pgspec_copyin FROM STDIN")
		c.ExpectCopyInResponse(t)

		// Send text rows.
		rows := []string{"1\tAlice\n", "2\tBob\n", "3\tCharlie\n"}
		for _, row := range rows {
			if err := c.Send(buildCopyData(w, []byte(row))); err != nil {
				t.Fatalf("send CopyData: %v", err)
			}
		}
		if err := c.Send(buildCopyDone(w)); err != nil {
			t.Fatalf("send CopyDone: %v", err)
		}

		tag := c.ExpectCommandComplete(t)
		assertTagPrefix(t, tag, "COPY 3")
		c.ExpectReadyForQuery(t)

		// Verify data.
		c.SendQuery("SELECT count(*) FROM pgspec_copyin")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "3" {
			t.Errorf("row count: got %q, want 3", fields[0])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("BinaryIn", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_copybin", "id int4")
		w := c.writer

		c.SendQuery("COPY pgspec_copybin FROM STDIN WITH (FORMAT binary)")
		c.ExpectCopyInResponse(t)

		// Build binary COPY stream.
		var stream []byte
		// Header.
		stream = append(stream, protocol.CopyBinaryHeader...)
		// Row: 1 field, int4, value=42.
		var row bytes.Buffer
		binary.Write(&row, binary.BigEndian, int16(1))  // field count
		binary.Write(&row, binary.BigEndian, int32(4))   // field length
		binary.Write(&row, binary.BigEndian, int32(42))  // value
		stream = append(stream, row.Bytes()...)
		// Trailer.
		stream = append(stream, protocol.CopyBinaryTrailer...)

		if err := c.Send(buildCopyData(w, stream)); err != nil {
			t.Fatalf("send binary CopyData: %v", err)
		}
		if err := c.Send(buildCopyDone(w)); err != nil {
			t.Fatalf("send CopyDone: %v", err)
		}

		tag := c.ExpectCommandComplete(t)
		assertTagPrefix(t, tag, "COPY 1")
		c.ExpectReadyForQuery(t)

		// Verify.
		c.SendQuery("SELECT id FROM pgspec_copybin")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "42" {
			t.Errorf("value: got %q, want 42", fields[0])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("Out", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_copyout", "id int, name text")

		// Insert data.
		c.SendQuery("INSERT INTO pgspec_copyout VALUES (1, 'Alice'), (2, 'Bob')")
		c.ReadUntilRFQ(t)

		// COPY TO STDOUT.
		c.SendQuery("COPY pgspec_copyout TO STDOUT")
		c.ExpectCopyOutResponse(t)

		// Read CopyData messages.
		var copyRows []string
		for {
			msgType, payload := c.ReadMsg(t)
			switch msgType {
			case protocol.BackendCopyData:
				copyRows = append(copyRows, string(payload))
			case protocol.BackendCopyDone:
				goto done
			default:
				t.Fatalf("unexpected message %q during COPY OUT", msgType)
			}
		}
	done:
		if len(copyRows) != 2 {
			t.Errorf("expected 2 copy rows, got %d", len(copyRows))
		}

		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("Fail", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_copyfail", "id int, name text")
		w := c.writer

		c.SendQuery("COPY pgspec_copyfail FROM STDIN")
		c.ExpectCopyInResponse(t)

		// Send one row, then fail.
		if err := c.Send(buildCopyData(w, []byte("1\tAlice\n"))); err != nil {
			t.Fatalf("send CopyData: %v", err)
		}
		if err := c.Send(buildCopyFail(w, "pgspec test abort")); err != nil {
			t.Fatalf("send CopyFail: %v", err)
		}

		// Expect ErrorResponse then ReadyForQuery.
		msgs := c.ReadUntilRFQ(t)
		if !hasMessage(msgs, protocol.BackendErrorResponse) {
			t.Error("expected ErrorResponse after CopyFail")
		}

		// Verify no data was committed.
		c.SendQuery("SELECT count(*) FROM pgspec_copyfail")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "0" {
			t.Errorf("row count after CopyFail: got %q, want 0", fields[0])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("WrongFormat", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_copywrong", "id int, name text")
		w := c.writer

		// Start a text-format COPY, but send binary header.
		c.SendQuery("COPY pgspec_copywrong FROM STDIN")
		c.ExpectCopyInResponse(t)

		// Send the binary magic header as if it were text data.
		if err := c.Send(buildCopyData(w, protocol.CopyBinaryHeader)); err != nil {
			t.Fatalf("send wrong format: %v", err)
		}
		if err := c.Send(buildCopyDone(w)); err != nil {
			t.Fatalf("send CopyDone: %v", err)
		}

		// Server should error because the data doesn't parse as tab-separated text.
		msgs := c.ReadUntilRFQ(t)
		if !hasMessage(msgs, protocol.BackendErrorResponse) {
			t.Error("expected ErrorResponse for wrong format COPY data")
		}
	})
}

// ============================================================================
// Section 6: Error Handling (55.2.6)
// ============================================================================

func TestError(t *testing.T) {
	t.Run("AllFields", func(t *testing.T) {
		c := dialPG(t)

		// Trigger an error with a known SQLSTATE.
		c.SendQuery("SELECT 1/0")
		msgs := c.ReadUntilRFQ(t)

		errMsgs := findMessages(msgs, protocol.BackendErrorResponse)
		if len(errMsgs) == 0 {
			t.Fatal("expected ErrorResponse for division by zero")
		}

		pgErr := protocol.ParseErrorResponse(errMsgs[0].Payload)
		if pgErr.Severity == "" {
			t.Error("missing Severity field")
		}
		if pgErr.Code == "" {
			t.Error("missing Code (SQLSTATE) field")
		}
		if pgErr.Message == "" {
			t.Error("missing Message field")
		}
		// Division by zero is SQLSTATE 22012.
		if pgErr.Code != "22012" {
			t.Errorf("SQLSTATE: got %q, want %q", pgErr.Code, "22012")
		}
		t.Logf("Error fields: S=%q C=%q M=%q D=%q H=%q Extra=%v",
			pgErr.Severity, pgErr.Code, pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Extra)
	})

	t.Run("Notice", func(t *testing.T) {
		c := dialPG(t)

		// Enable notice output.
		c.SendQuery("SET client_min_messages TO notice")
		c.ReadUntilRFQ(t)

		// DO block that raises a notice.
		c.SendQuery("DO $$ BEGIN RAISE NOTICE 'pgspec test notice'; END $$")
		msgs := c.ReadUntilRFQ(t)

		noticeMsgs := findMessages(msgs, protocol.BackendNoticeResponse)
		if len(noticeMsgs) == 0 {
			t.Error("expected NoticeResponse from RAISE NOTICE")
		} else {
			notice := protocol.ParseErrorResponse(noticeMsgs[0].Payload)
			if !strings.Contains(notice.Message, "pgspec test notice") {
				t.Errorf("notice message: got %q", notice.Message)
			}
		}
	})

	t.Run("RecoveryAfterError", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Extended query with error.
		var buf []byte
		buf = append(buf, buildParse(w, "", "SELECT 1/0", nil)...)
		buf = append(buf, buildBind(w, "", "", nil, nil, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		// Drain until RFQ; there should be an error.
		status := c.drainUntilRFQ(t)
		if status != 'I' {
			t.Errorf("RFQ status after error: got %q, want 'I'", status)
		}

		// Connection should be recovered.
		c.SendQuery("SELECT 42 AS recovered")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "42" {
			t.Errorf("recovery value: got %q, want %q", fields[0], "42")
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})
}

// ============================================================================
// Section 7: Wire Framing Edge Cases
// ============================================================================

func TestFrame(t *testing.T) {
	t.Run("ZeroLengthPayload", func(t *testing.T) {
		c := dialPG(t)
		w := c.writer

		// Sync and Flush have no payload beyond the header.
		var buf []byte
		buf = append(buf, buildFlush(w)...)
		buf = append(buf, buildParse(w, "", "SELECT 1", nil)...)
		buf = append(buf, buildBind(w, "", "", nil, nil, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send: %v", err)
		}

		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		c.ExpectDataRow(t)
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("SplitReads", func(t *testing.T) {
		c := dialPG(t)

		// Build a Query message and send it one byte at a time.
		data := protocol.WriteQuery(c.writer, "SELECT 'split_test' AS s")
		for i, b := range data {
			if _, err := c.conn.Write([]byte{b}); err != nil {
				t.Fatalf("write byte %d: %v", i, err)
			}
		}

		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "split_test" {
			t.Errorf("split read value: got %q", fields[0])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("BackToBack", func(t *testing.T) {
		c := dialPG(t)
		c.SetDeadline(30 * time.Second)

		// Send 100 rapid-fire queries without reading responses.
		var allData []byte
		for i := range 100 {
			q := protocol.WriteQuery(c.writer, fmt.Sprintf("SELECT %d AS n", i))
			allData = append(allData, q...)
		}
		if err := c.Send(allData); err != nil {
			t.Fatalf("send batch: %v", err)
		}

		// Now read all responses.
		for i := range 100 {
			msgs := c.ReadUntilRFQ(t)
			if !hasMessage(msgs, protocol.BackendDataRow) {
				t.Fatalf("query %d: no DataRow", i)
			}
			dataRows := findMessages(msgs, protocol.BackendDataRow)
			fields, err := protocol.ParseDataRow(dataRows[0].Payload)
			if err != nil {
				t.Fatalf("query %d: ParseDataRow: %v", i, err)
			}
			expected := strconv.Itoa(i)
			if string(fields[0]) != expected {
				t.Errorf("query %d: got %q, want %q", i, fields[0], expected)
			}
		}
	})
}

// ============================================================================
// Section 8: Type Round-trips
// ============================================================================

func TestTypes(t *testing.T) {
	t.Run("AllBuiltins", func(t *testing.T) {
		c := dialPG(t)
		createTempTable(t, c, "pgspec_types", `
			c_bool bool,
			c_int2 int2,
			c_int4 int4,
			c_int8 int8,
			c_float4 float4,
			c_float8 float8,
			c_text text,
			c_bytea bytea,
			c_date date,
			c_timestamp timestamp,
			c_timestamptz timestamptz,
			c_uuid uuid,
			c_jsonb jsonb,
			c_numeric numeric
		`)

		// Insert via simple query.
		c.SendQuery(`INSERT INTO pgspec_types VALUES (
			true, 32767, 2147483647, 9223372036854775807,
			3.14, 2.718281828459045,
			'hello pgspec', '\xDEADBEEF',
			'2024-06-15', '2024-06-15 12:30:00', '2024-06-15 12:30:00+00',
			'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11',
			'{"key":"value"}',
			'12345.6789'
		)`)
		c.ReadUntilRFQ(t)

		// Select back and verify round-trip.
		c.SendQuery("SELECT * FROM pgspec_types")
		cols := c.ExpectRowDescription(t)
		if len(cols) != 14 {
			t.Fatalf("expected 14 columns, got %d", len(cols))
		}

		fields := c.ExpectDataRow(t)
		if len(fields) != 14 {
			t.Fatalf("expected 14 fields, got %d", len(fields))
		}

		expected := map[int]string{
			0:  "t",                                      // bool
			1:  "32767",                                  // int2
			2:  "2147483647",                             // int4
			3:  "9223372036854775807",                    // int8
			6:  "hello pgspec",                           // text
			7:  "\\xdeadbeef",                            // bytea
			8:  "2024-06-15",                             // date
			13: "12345.6789",                             // numeric
		}
		for idx, want := range expected {
			got := string(fields[idx])
			if got != want {
				t.Errorf("column %d (%s): got %q, want %q", idx, cols[idx].Name, got, want)
			}
		}

		// Float checks (allow minor formatting differences).
		f4 := string(fields[4])
		if !strings.HasPrefix(f4, "3.14") {
			t.Errorf("float4: got %q", f4)
		}
		f8 := string(fields[5])
		if !strings.HasPrefix(f8, "2.71828") {
			t.Errorf("float8: got %q", f8)
		}

		// UUID.
		uuid := string(fields[11])
		if uuid != "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11" {
			t.Errorf("uuid: got %q", uuid)
		}

		// JSONB.
		jsonb := string(fields[12])
		if !strings.Contains(jsonb, "key") || !strings.Contains(jsonb, "value") {
			t.Errorf("jsonb: got %q", jsonb)
		}

		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("NULL", func(t *testing.T) {
		c := dialPG(t)

		types := []string{"bool", "int2", "int4", "int8", "float4", "float8",
			"text", "bytea", "date", "timestamp", "timestamptz", "uuid", "jsonb", "numeric"}
		var casts []string
		for _, typ := range types {
			casts = append(casts, fmt.Sprintf("NULL::%s", typ))
		}
		sql := "SELECT " + strings.Join(casts, ", ")

		c.SendQuery(sql)
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		for i, f := range fields {
			if f != nil {
				t.Errorf("type %s (col %d): expected NULL, got %q", types[i], i, f)
			}
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("Arrays", func(t *testing.T) {
		c := dialPG(t)

		c.SendQuery("SELECT ARRAY[1,2,3]::int[] AS iarr, ARRAY['a','b','c']::text[] AS tarr")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if len(fields) != 2 {
			t.Fatalf("expected 2 fields, got %d", len(fields))
		}
		if string(fields[0]) != "{1,2,3}" {
			t.Errorf("int array: got %q", fields[0])
		}
		if string(fields[1]) != "{a,b,c}" {
			t.Errorf("text array: got %q", fields[1])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("LargeValues", func(t *testing.T) {
		c := dialPG(t)
		c.SetDeadline(60 * time.Second)
		createTempTable(t, c, "pgspec_large", "big_text text, big_bytea bytea")

		// 1MB text.
		bigText := strings.Repeat("x", 1024*1024)
		// Use extended query for large params.
		w := c.writer
		var buf []byte
		buf = append(buf, buildParse(w, "", "INSERT INTO pgspec_large VALUES ($1, $2)", nil)...)

		// 1MB bytea as hex.
		bigBytea := make([]byte, 1024*1024)
		for i := range bigBytea {
			bigBytea[i] = byte(i % 256)
		}
		var hexBytea strings.Builder
		hexBytea.WriteString("\\x")
		for _, b := range bigBytea {
			fmt.Fprintf(&hexBytea, "%02x", b)
		}

		buf = append(buf, buildBind(w, "", "", []int16{0, 0}, [][]byte{[]byte(bigText), []byte(hexBytea.String())}, nil)...)
		buf = append(buf, buildExecute(w, "", 0)...)
		buf = append(buf, buildSync(w)...)
		if err := c.Send(buf); err != nil {
			t.Fatalf("send large insert: %v", err)
		}

		c.ExpectParseComplete(t)
		c.ExpectBindComplete(t)
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)

		// Read back and verify text length.
		c.SendQuery("SELECT length(big_text), length(big_bytea) FROM pgspec_large")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "1048576" {
			t.Errorf("text length: got %q, want 1048576", fields[0])
		}
		if string(fields[1]) != "1048576" {
			t.Errorf("bytea length: got %q, want 1048576", fields[1])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})
}

// ============================================================================
// Section 9: Connection Lifecycle
// ============================================================================

func TestLifecycle(t *testing.T) {
	t.Run("Terminate", func(t *testing.T) {
		c := dialPG(t)

		// Send Terminate.
		c.SendTerminate()

		// Server should close the connection.
		c.SetDeadline(5 * time.Second)
		_, _, err := c.readMsg()
		if err == nil {
			t.Error("expected connection closed after Terminate")
		}
	})

	t.Run("IdleTimeout", func(t *testing.T) {
		c := dialPG(t)

		// Hold the connection idle for 2 seconds (should not be dropped by default PG config).
		time.Sleep(2 * time.Second)

		// Verify connection is still alive.
		c.SetDeadline(10 * time.Second)
		c.SendQuery("SELECT 'still alive' AS status")
		_ = c.ExpectRowDescription(t)
		fields := c.ExpectDataRow(t)
		if string(fields[0]) != "still alive" {
			t.Errorf("after idle: got %q", fields[0])
		}
		c.ExpectCommandComplete(t)
		c.ExpectReadyForQuery(t)
	})

	t.Run("Cancel", func(t *testing.T) {
		c := dialPG(t)
		pid := c.PID
		secret := c.Secret

		// Start a long query.
		c.SendQuery("SELECT pg_sleep(30)")

		// Give the query a moment to start.
		time.Sleep(200 * time.Millisecond)

		// Open a separate connection and send CancelRequest.
		info := parseDSNInfo(t)
		cancelConn, err := dialTCP(t, info)
		if err != nil {
			t.Fatalf("dial for cancel: %v", err)
		}
		defer cancelConn.Close()

		cancelWriter := protocol.NewWriter()
		cancelWriter.Reset()
		cancelWriter.StartStartupMessage()
		cancelWriter.WriteInt32(80877102)
		cancelWriter.WriteInt32(pid)
		cancelWriter.WriteInt32(secret)
		cancelWriter.FinishMessage()
		if err := cancelConn.Send(cancelWriter.Bytes()); err != nil {
			t.Fatalf("send cancel: %v", err)
		}

		// The main connection should get an error and then RFQ.
		c.SetDeadline(10 * time.Second)
		msgs := c.ReadUntilRFQ(t)
		if !hasMessage(msgs, protocol.BackendErrorResponse) {
			t.Error("expected ErrorResponse after cancel")
		} else {
			errMsgs := findMessages(msgs, protocol.BackendErrorResponse)
			pgErr := protocol.ParseErrorResponse(errMsgs[0].Payload)
			// SQLSTATE 57014 = query_canceled.
			if pgErr.Code != "57014" {
				t.Logf("cancel SQLSTATE: %s (expected 57014)", pgErr.Code)
			}
		}
	})
}

// dialTCP opens a raw TCP connection to the PG server.
func dialTCP(t *testing.T, info pgConnInfo) (*pgRawConn, error) {
	t.Helper()
	addr := info.Host + ":" + info.Port
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &pgRawConn{
		conn:   conn,
		reader: protocol.NewReader(),
		writer: protocol.NewWriter(),
		info:   info,
	}, nil
}

// ============================================================================
// Section 10: Extended Query Edge Cases (C-findings)
// ============================================================================

func TestExtended_ErrorDuringBind(t *testing.T) {
	c := dialPG(t)
	w := c.writer

	// Parse a statement with one parameter, then Bind with zero params.
	var buf []byte
	buf = append(buf, buildParse(w, "", "SELECT $1::int AS val", nil)...)
	buf = append(buf, buildBind(w, "", "", nil, nil, nil)...) // wrong param count
	buf = append(buf, buildSync(w)...)
	if err := c.Send(buf); err != nil {
		t.Fatalf("send: %v", err)
	}

	// ParseComplete should succeed, then ErrorResponse for the bad Bind.
	c.ExpectParseComplete(t)
	msgs := c.ReadUntilRFQ(t)
	if !hasMessage(msgs, protocol.BackendErrorResponse) {
		t.Error("expected ErrorResponse for bind with wrong param count")
	}

	// Connection must recover.
	c.SendQuery("SELECT 1")
	status := c.drainUntilRFQ(t)
	if status != 'I' {
		t.Errorf("RFQ status after recovery: got %q, want 'I'", status)
	}
}

func TestExtended_PortalSuspendedVerified(t *testing.T) {
	c := dialPG(t)
	w := c.writer

	// Execute with explicit maxRows=5, verify PortalSuspended, then fetch more.
	var buf []byte
	buf = append(buf, buildParse(w, "", "SELECT generate_series(1, 20)::text AS n", nil)...)
	buf = append(buf, buildBind(w, "ps_verified", "", nil, nil, nil)...)
	buf = append(buf, buildExecute(w, "ps_verified", 5)...)
	buf = append(buf, buildFlush(w)...)
	if err := c.Send(buf); err != nil {
		t.Fatalf("send: %v", err)
	}

	c.ExpectParseComplete(t)
	c.ExpectBindComplete(t)

	// Read exactly 5 rows.
	for i := 1; i <= 5; i++ {
		f := c.ExpectDataRow(t)
		if string(f[0]) != strconv.Itoa(i) {
			t.Errorf("row %d: got %q, want %q", i, f[0], strconv.Itoa(i))
		}
	}
	c.ExpectPortalSuspended(t)

	// Fetch the next 5.
	buf = buf[:0]
	buf = append(buf, buildExecute(w, "ps_verified", 5)...)
	buf = append(buf, buildFlush(w)...)
	if err := c.Send(buf); err != nil {
		t.Fatalf("send execute: %v", err)
	}

	for i := 6; i <= 10; i++ {
		f := c.ExpectDataRow(t)
		if string(f[0]) != strconv.Itoa(i) {
			t.Errorf("row %d: got %q, want %q", i, f[0], strconv.Itoa(i))
		}
	}
	c.ExpectPortalSuspended(t)

	// Sync to finalize.
	buf = buf[:0]
	buf = append(buf, buildSync(w)...)
	if err := c.Send(buf); err != nil {
		t.Fatalf("send sync: %v", err)
	}
	c.ExpectReadyForQuery(t)
}

func TestSimpleQuery_TransactionStatusByte(t *testing.T) {
	c := dialPG(t)

	// Verify 'I' after a normal query.
	c.SendQuery("SELECT 1")
	status := c.drainUntilRFQ(t)
	if status != 'I' {
		t.Errorf("after SELECT: got %q, want 'I'", status)
	}

	// Verify 'T' after BEGIN.
	c.SendQuery("BEGIN")
	status = c.drainUntilRFQ(t)
	if status != 'T' {
		t.Errorf("after BEGIN: got %q, want 'T'", status)
	}

	// Verify 'E' after a failed query in a transaction.
	c.SendQuery("SELECT FROM nonexistent_pgspec_txstatus_table_xyz")
	status = c.drainUntilRFQ(t)
	if status != 'E' {
		t.Errorf("after failed query in tx: got %q, want 'E'", status)
	}

	// ROLLBACK to return to idle.
	c.SendQuery("ROLLBACK")
	status = c.drainUntilRFQ(t)
	if status != 'I' {
		t.Errorf("after ROLLBACK: got %q, want 'I'", status)
	}
}

func TestTypes_TimestampInfinity(t *testing.T) {
	c := dialPG(t)
	w := c.writer

	// Use extended query with binary result format to test infinity encoding.
	var buf []byte
	buf = append(buf, buildParse(w, "", "SELECT 'infinity'::timestamptz AS pos, '-infinity'::timestamptz AS neg", nil)...)
	buf = append(buf, buildBind(w, "", "", nil, nil, []int16{1, 1})...) // binary result
	buf = append(buf, buildDescribe(w, 'P', "")...)
	buf = append(buf, buildExecute(w, "", 0)...)
	buf = append(buf, buildSync(w)...)
	if err := c.Send(buf); err != nil {
		t.Fatalf("send: %v", err)
	}

	c.ExpectParseComplete(t)
	c.ExpectBindComplete(t)
	cols := c.ExpectRowDescription(t)
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}

	fields := c.ExpectDataRow(t)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	// In binary format, infinity is 8 bytes: int64 max (0x7FFFFFFFFFFFFFFF).
	// -infinity is int64 min (0x8000000000000000).
	if len(fields[0]) != 8 {
		t.Errorf("pos infinity: expected 8 bytes, got %d", len(fields[0]))
	}
	if len(fields[1]) != 8 {
		t.Errorf("neg infinity: expected 8 bytes, got %d", len(fields[1]))
	}
	// Verify the raw int64 values.
	posVal := int64(binary.BigEndian.Uint64(fields[0]))
	negVal := int64(binary.BigEndian.Uint64(fields[1]))
	if posVal != 0x7FFFFFFFFFFFFFFF {
		t.Errorf("pos infinity: got 0x%016X, want 0x7FFFFFFFFFFFFFFF", uint64(posVal))
	}
	if negVal != -0x8000000000000000 {
		t.Errorf("neg infinity: got 0x%016X, want 0x8000000000000000", uint64(negVal))
	}

	c.ExpectCommandComplete(t)
	c.ExpectReadyForQuery(t)
}

func TestTypes_DateInfinity(t *testing.T) {
	c := dialPG(t)
	w := c.writer

	var buf []byte
	buf = append(buf, buildParse(w, "", "SELECT 'infinity'::date AS pos, '-infinity'::date AS neg", nil)...)
	buf = append(buf, buildBind(w, "", "", nil, nil, []int16{1, 1})...) // binary result
	buf = append(buf, buildDescribe(w, 'P', "")...)
	buf = append(buf, buildExecute(w, "", 0)...)
	buf = append(buf, buildSync(w)...)
	if err := c.Send(buf); err != nil {
		t.Fatalf("send: %v", err)
	}

	c.ExpectParseComplete(t)
	c.ExpectBindComplete(t)
	cols := c.ExpectRowDescription(t)
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}

	fields := c.ExpectDataRow(t)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	// In binary format, date infinity is 4 bytes: int32 max (0x7FFFFFFF).
	// -infinity is int32 min (0x80000000).
	if len(fields[0]) != 4 {
		t.Errorf("pos date infinity: expected 4 bytes, got %d", len(fields[0]))
	}
	if len(fields[1]) != 4 {
		t.Errorf("neg date infinity: expected 4 bytes, got %d", len(fields[1]))
	}
	posVal := int32(binary.BigEndian.Uint32(fields[0]))
	negVal := int32(binary.BigEndian.Uint32(fields[1]))
	if posVal != 0x7FFFFFFF {
		t.Errorf("pos date infinity: got 0x%08X, want 0x7FFFFFFF", uint32(posVal))
	}
	if negVal != -0x80000000 {
		t.Errorf("neg date infinity: got 0x%08X, want 0x80000000", uint32(negVal))
	}

	c.ExpectCommandComplete(t)
	c.ExpectReadyForQuery(t)
}

func TestCopy_LargeOut(t *testing.T) {
	c := dialPG(t)
	c.SetDeadline(60 * time.Second)
	createTempTable(t, c, "pgspec_copylargeout", "id int, val text")

	// Insert 10K rows.
	c.SendQuery("INSERT INTO pgspec_copylargeout SELECT i, 'row_' || i FROM generate_series(1, 10000) AS i")
	c.ReadUntilRFQ(t)

	// COPY TO STDOUT.
	c.SendQuery("COPY pgspec_copylargeout TO STDOUT")
	c.ExpectCopyOutResponse(t)

	rowCount := 0
	for {
		msgType, _ := c.ReadMsg(t)
		switch msgType {
		case protocol.BackendCopyData:
			rowCount++
		case protocol.BackendCopyDone:
			goto done
		default:
			t.Fatalf("unexpected message %q during COPY OUT", msgType)
		}
	}
done:
	if rowCount != 10000 {
		t.Errorf("COPY OUT row count: got %d, want 10000", rowCount)
	}

	c.ExpectCommandComplete(t)
	c.ExpectReadyForQuery(t)
}
