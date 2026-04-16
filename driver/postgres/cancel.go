package postgres

import (
	"context"
	"encoding/binary"
	"net"
	"time"
)

// cancelRequestCode is the magic int32 that identifies a CancelRequest, from
// the PostgreSQL v3 protocol docs (80877102 = (1234 << 16) | 5678).
const cancelRequestCode int32 = 80877102

// buildCancelRequest produces the 16-byte CancelRequest payload:
//
//	int32 length=16, int32 code=cancelRequestCode, int32 pid, int32 secret.
//
// Exposed for testing.
func buildCancelRequest(pid, secret int32) []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:4], 16)
	binary.BigEndian.PutUint32(buf[4:8], uint32(cancelRequestCode))
	binary.BigEndian.PutUint32(buf[8:12], uint32(pid))
	binary.BigEndian.PutUint32(buf[12:16], uint32(secret))
	return buf
}

// sendCancelRequest dials a fresh short-lived TCP connection to addr and
// transmits a CancelRequest. Does not use the event loop — cancellation is
// fire-and-forget: we don't wait for a reply, the server simply closes the
// socket after processing.
//
// If ctx has a deadline, it is enforced on both dial and write. The function
// never blocks waiting for a server response.
func sendCancelRequest(_ context.Context, addr *net.TCPAddr, pid, secret int32) error {
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(cancelCtx, "tcp", addr.String())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := cancelCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	_, err = conn.Write(buildCancelRequest(pid, secret))
	return err
}
