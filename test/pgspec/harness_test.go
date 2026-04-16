//go:build pgspec

package pgspec

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

const envDSN = "CELERIS_PG_DSN"

// pgConnInfo holds parsed DSN fields needed for raw TCP connections.
type pgConnInfo struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

func parseDSNInfo(t *testing.T) pgConnInfo {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envDSN))
	if raw == "" {
		t.Skipf("skipping: %s not set", envDSN)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	info := pgConnInfo{
		Host: u.Hostname(),
		Port: u.Port(),
	}
	if info.Host == "" {
		info.Host = "127.0.0.1"
	}
	if info.Port == "" {
		info.Port = "5432"
	}
	if u.User != nil {
		info.User = u.User.Username()
		info.Password, _ = u.User.Password()
	}
	if info.User == "" {
		info.User = "postgres"
	}
	info.Database = strings.TrimPrefix(u.Path, "/")
	if info.Database == "" {
		info.Database = info.User
	}
	return info
}

// pgRawConn wraps a TCP connection with helpers for reading and writing
// PostgreSQL wire protocol messages.
type pgRawConn struct {
	conn   net.Conn
	reader *protocol.Reader
	writer *protocol.Writer
	info   pgConnInfo

	// Populated by startup handshake.
	PID    int32
	Secret int32
}

// dialRaw opens a raw TCP connection without performing the startup handshake.
func dialRaw(t *testing.T) *pgRawConn {
	t.Helper()
	info := parseDSNInfo(t)
	addr := net.JoinHostPort(info.Host, info.Port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c := &pgRawConn{
		conn:   conn,
		reader: protocol.NewReader(),
		writer: protocol.NewWriter(),
		info:   info,
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dialPG opens a raw TCP connection and completes the startup handshake.
func dialPG(t *testing.T) *pgRawConn {
	t.Helper()
	c := dialRaw(t)
	c.startup(t)
	return c
}

// Close closes the underlying TCP connection.
func (c *pgRawConn) Close() error {
	return c.conn.Close()
}

// Send writes raw bytes to the connection.
func (c *pgRawConn) Send(data []byte) error {
	_, err := c.conn.Write(data)
	return err
}

// SendMessage sends a single protocol message.
func (c *pgRawConn) SendMessage(msgType byte, build func(w *protocol.Writer)) {
	c.writer.Reset()
	c.writer.StartMessage(msgType)
	if build != nil {
		build(c.writer)
	}
	c.writer.FinishMessage()
	if _, err := c.conn.Write(c.writer.Bytes()); err != nil {
		panic(fmt.Sprintf("pgRawConn.SendMessage: %v", err))
	}
}

// SendStartupMessage sends a startup-style message (no type byte).
func (c *pgRawConn) SendStartupMessage(build func(w *protocol.Writer)) {
	c.writer.Reset()
	c.writer.StartStartupMessage()
	build(c.writer)
	c.writer.FinishMessage()
	if _, err := c.conn.Write(c.writer.Bytes()); err != nil {
		panic(fmt.Sprintf("pgRawConn.SendStartupMessage: %v", err))
	}
}

// readMsg reads one complete backend message. It returns the message type
// byte and payload.
func (c *pgRawConn) readMsg() (byte, []byte, error) {
	for {
		msgType, payload, err := c.reader.Next()
		if err == nil {
			return msgType, payload, nil
		}
		if err != protocol.ErrIncomplete {
			return 0, nil, err
		}
		buf := make([]byte, 4096)
		n, readErr := c.conn.Read(buf)
		if n > 0 {
			c.reader.Feed(buf[:n])
		}
		if readErr != nil {
			// Try once more with whatever is buffered.
			msgType, payload, err = c.reader.Next()
			if err == nil {
				return msgType, payload, nil
			}
			return 0, nil, readErr
		}
	}
}

// ReadMsg reads one message, failing the test on error.
func (c *pgRawConn) ReadMsg(t *testing.T) (byte, []byte) {
	t.Helper()
	msgType, payload, err := c.readMsg()
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	return msgType, payload
}

// pgMessage is a captured backend message.
type pgMessage struct {
	Type    byte
	Payload []byte
}

// ReadUntilRFQ reads messages until ReadyForQuery is received. It returns
// all messages including the final ReadyForQuery.
func (c *pgRawConn) ReadUntilRFQ(t *testing.T) []pgMessage {
	t.Helper()
	var msgs []pgMessage
	for {
		msgType, payload := c.ReadMsg(t)
		// Make an owned copy of the payload.
		owned := make([]byte, len(payload))
		copy(owned, payload)
		msgs = append(msgs, pgMessage{Type: msgType, Payload: owned})
		if msgType == protocol.BackendReadyForQuery {
			return msgs
		}
	}
}

// SendQuery sends a Query ('Q') message.
func (c *pgRawConn) SendQuery(sql string) {
	data := protocol.WriteQuery(c.writer, sql)
	if err := c.Send(data); err != nil {
		panic(fmt.Sprintf("SendQuery: %v", err))
	}
}

// SendSync sends a Sync ('S') message.
func (c *pgRawConn) SendSync() {
	data := protocol.WriteSync(c.writer)
	if err := c.Send(data); err != nil {
		panic(fmt.Sprintf("SendSync: %v", err))
	}
}

// SendTerminate sends a Terminate ('X') message.
func (c *pgRawConn) SendTerminate() {
	c.SendMessage(protocol.MsgTerminate, nil)
}

// startup performs the v3.0 startup handshake using the StartupState machine.
func (c *pgRawConn) startup(t *testing.T) {
	t.Helper()
	state := &protocol.StartupState{
		User:     c.info.User,
		Password: c.info.Password,
		Database: c.info.Database,
	}
	startBytes := state.Start(c.writer)
	if err := c.Send(startBytes); err != nil {
		t.Fatalf("startup send: %v", err)
	}

	for {
		msgType, payload := c.ReadMsg(t)
		resp, done, err := state.Handle(msgType, payload, c.writer)
		if err != nil {
			t.Fatalf("startup handle: %v", err)
		}
		if resp != nil {
			if err := c.Send(resp); err != nil {
				t.Fatalf("startup send response: %v", err)
			}
		}
		if done {
			c.PID = state.PID
			c.Secret = state.Secret
			return
		}
	}
}

// SetDeadline sets the deadline on the underlying connection.
func (c *pgRawConn) SetDeadline(d time.Duration) {
	_ = c.conn.SetDeadline(time.Now().Add(d))
}

// sendSSLRequest sends an SSLRequest message (code 80877103).
func (c *pgRawConn) sendSSLRequest() error {
	c.writer.Reset()
	c.writer.StartStartupMessage()
	c.writer.WriteInt32(80877103) // SSLRequest code
	c.writer.FinishMessage()
	return c.Send(c.writer.Bytes())
}

// sendCancelRequest sends a CancelRequest message.
func (c *pgRawConn) sendCancelRequest(pid, secret int32) error {
	c.writer.Reset()
	c.writer.StartStartupMessage()
	c.writer.WriteInt32(80877102) // CancelRequest code
	c.writer.WriteInt32(pid)
	c.writer.WriteInt32(secret)
	c.writer.FinishMessage()
	return c.Send(c.writer.Bytes())
}

// sendStartupV2 sends a v2.0 StartupMessage.
func (c *pgRawConn) sendStartupV2() error {
	c.writer.Reset()
	c.writer.StartStartupMessage()
	c.writer.WriteInt32(2 << 16) // v2.0
	c.writer.WriteString("user")
	c.writer.WriteString(c.info.User)
	_ = c.writer.WriteByte(0)
	c.writer.FinishMessage()
	return c.Send(c.writer.Bytes())
}

// sendStartupV3 sends a v3.0 StartupMessage with user, database, and extra params.
func (c *pgRawConn) sendStartupV3(user, database string, params map[string]string) error {
	c.writer.Reset()
	c.writer.StartStartupMessage()
	c.writer.WriteInt32(protocol.ProtocolVersion) // v3.0
	c.writer.WriteString("user")
	c.writer.WriteString(user)
	if database != "" {
		c.writer.WriteString("database")
		c.writer.WriteString(database)
	}
	for k, v := range params {
		c.writer.WriteString(k)
		c.writer.WriteString(v)
	}
	_ = c.writer.WriteByte(0) // param list terminator
	c.writer.FinishMessage()
	return c.Send(c.writer.Bytes())
}

// readOneByte reads exactly one byte from the connection.
func (c *pgRawConn) readOneByte(t *testing.T) byte {
	t.Helper()
	buf := make([]byte, 1)
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadFull(c.conn, buf)
	if err != nil {
		t.Fatalf("readOneByte: %v", err)
	}
	return buf[0]
}

// readRawBytes reads exactly n bytes from the connection.
func (c *pgRawConn) readRawBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadFull(c.conn, buf)
	if err != nil {
		t.Fatalf("readRawBytes(%d): %v", n, err)
	}
	return buf
}

// readRawMsg reads one backend message as raw bytes (type + length + payload)
// without going through the Reader. Used when we need the raw framing.
func (c *pgRawConn) readRawMsg(t *testing.T) (byte, []byte) {
	t.Helper()
	// type byte
	hdr := c.readRawBytes(t, 5)
	msgType := hdr[0]
	length := int(binary.BigEndian.Uint32(hdr[1:5]))
	if length < 4 {
		t.Fatalf("readRawMsg: invalid length %d", length)
	}
	payload := make([]byte, length-4)
	if len(payload) > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			t.Fatalf("readRawMsg payload: %v", err)
		}
	}
	return msgType, payload
}

// expectConnClosed verifies that the server has closed the connection.
func (c *pgRawConn) expectConnClosed(t *testing.T) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err := c.conn.Read(buf)
	if err == nil {
		t.Error("expected connection closed, but read succeeded")
	}
}
