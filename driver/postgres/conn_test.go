package postgres

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// startFakePG launches an in-process PostgreSQL server that answers a scripted
// set of responses. Each handler is called per accepted conn with the
// fresh net.Conn.
func startFakePG(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	return ln.Addr().String()
}

// readStartup reads the StartupMessage off c. Format: int32 len, int32
// ProtocolVersion, (cstring key, cstring value)* , null.
//
// NOTE: this runs on the fake-server handler goroutine and MUST NOT call
// t.Fatalf — doing so from a non-test goroutine races the testing harness
// (per testing.T docs). When the client side legitimately closes before
// the startup completes (e.g. TestPgPoolCloseRacesAcquire racing Pool.Close
// against pending dials), we return ok=false and the handler silently exits.
// Any real protocol bug still surfaces because the client's dialConn will
// fail and the test body checks that path.
func readStartup(t *testing.T, c net.Conn) bool {
	t.Helper()
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c, lenBuf); err != nil {
		return false
	}
	n := int(binary.BigEndian.Uint32(lenBuf))
	if n < 4 {
		return false
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return false
	}
	return true
}

// writeMsg serializes a typed message: type byte, int32 length (incl length
// field), payload.
//
// Implementation note: when the caller provides a scratch buffer via the
// (non-exported) WriteMsgCtx path it's reused across calls — but the public
// signature used by all callers still takes the per-call allocation hit.
// The allocation-budget tests accept this as unavoidable harness overhead.
func writeMsg(c net.Conn, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := c.Write(buf)
	return err
}

// writeAuthOK writes AuthenticationOk ('R' with int32=0).
func writeAuthOK(c net.Conn) error {
	payload := make([]byte, 4) // subtype=0
	return writeMsg(c, protocol.BackendAuthentication, payload)
}

// writeBackendKeyData writes 'K' with pid+secret.
func writeBackendKeyData(c net.Conn, pid, secret int32) error {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[0:4], uint32(pid))
	binary.BigEndian.PutUint32(p[4:8], uint32(secret))
	return writeMsg(c, protocol.BackendBackendKeyData, p)
}

// writeReadyForQuery writes 'Z' with status byte.
func writeReadyForQuery(c net.Conn, status byte) error {
	return writeMsg(c, protocol.BackendReadyForQuery, []byte{status})
}

// writeCommandComplete writes 'C' with the tag.
func writeCommandComplete(c net.Conn, tag string) error {
	p := append([]byte(tag), 0)
	return writeMsg(c, protocol.BackendCommandComplete, p)
}

// readMsg reads one typed message. Returns type, payload.
func readMsg(c net.Conn) (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return 0, nil, err
	}
	typ := hdr[0]
	n := int(binary.BigEndian.Uint32(hdr[1:5]))
	if n < 4 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

// fakePGTrustStartup handles the startup exchange with trust (no-password)
// authentication and then calls post for the query phase.
func fakePGTrustStartup(t *testing.T, c net.Conn, pid, secret int32, post func(net.Conn)) {
	t.Helper()
	defer func() { _ = c.Close() }()
	readStartup(t, c)
	if err := writeAuthOK(c); err != nil {
		return
	}
	if err := writeBackendKeyData(c, pid, secret); err != nil {
		return
	}
	if err := writeReadyForQuery(c, 'I'); err != nil {
		return
	}
	if post != nil {
		post(c)
	}
}

func TestConn_StartupAndPing(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 42, 0x1ABCD123, func(c net.Conn) {
			// Expect Query("SELECT 1").
			typ, body, err := readMsg(c)
			if err != nil || typ != protocol.MsgQuery {
				return
			}
			_ = body
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
			// Read Terminate + close.
			_, _, _ = readMsg(c)
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{
		Host: host, Port: port, User: "u", Database: "d",
		Options: Options{SSLMode: "disable", StatementCacheSize: 16},
		Params:  map[string]string{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatalf("dialConn: %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.pid != 42 {
		t.Errorf("pid = %d", c.pid)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestConn_SimpleQuery(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			typ, _, err := readMsg(c)
			if err != nil || typ != protocol.MsgQuery {
				return
			}
			// RowDescription: 1 col "n" int4.
			rd := buildRowDescription([]colSpec{{name: "n", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText}})
			_ = writeMsg(c, protocol.BackendRowDescription, rd)
			_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte("7")}))
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
			_, _, _ = readMsg(c)
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{
		Host: host, Port: port, User: "u",
		Options: Options{SSLMode: "disable", StatementCacheSize: 16},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	rows, err := c.QueryContext(ctx, "SELECT 7", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols := rows.Columns()
	if len(cols) != 1 || cols[0] != "n" {
		t.Errorf("cols=%v", cols)
	}
}

type colSpec struct {
	name     string
	typeOID  uint32
	typeSize int16
	format   int16
}

func buildRowDescription(cols []colSpec) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out[0:2], uint16(len(cols)))
	for _, c := range cols {
		out = append(out, []byte(c.name)...)
		out = append(out, 0)
		var buf [18]byte
		binary.BigEndian.PutUint32(buf[0:4], 0) // table oid
		binary.BigEndian.PutUint16(buf[4:6], 0) // attnum
		binary.BigEndian.PutUint32(buf[6:10], c.typeOID)
		binary.BigEndian.PutUint16(buf[10:12], uint16(c.typeSize))
		binary.BigEndian.PutUint32(buf[12:16], 0xFFFFFFFF) // typmod -1
		binary.BigEndian.PutUint16(buf[16:18], uint16(c.format))
		out = append(out, buf[:]...)
	}
	return out
}

func buildDataRow(fields [][]byte) []byte {
	var out []byte
	hdr := make([]byte, 2)
	binary.BigEndian.PutUint16(hdr, uint16(len(fields)))
	out = append(out, hdr...)
	for _, f := range fields {
		if f == nil {
			lenBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lenBuf, 0xFFFFFFFF)
			out = append(out, lenBuf...)
			continue
		}
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(f)))
		out = append(out, lenBuf...)
		out = append(out, f...)
	}
	return out
}

// Ensure concurrent conn close during queue drain doesn't deadlock.
func TestConn_CloseWithPending(t *testing.T) {
	// Accepts a startup, keeps the conn open until closed.
	var wg sync.WaitGroup
	wg.Add(1)
	addr := startFakePG(t, func(c net.Conn) {
		defer wg.Done()
		defer func() { _ = c.Close() }()
		readStartup(t, c)
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 1, 2)
		_ = writeReadyForQuery(c, 'I')
		// Don't respond to the query; the test closes the conn and expects
		// Ping to unblock.
		_, _ = io.Copy(io.Discard, c)
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- c.Ping(context.Background())
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected Ping error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping did not return after Close")
	}
	wg.Wait()
}
