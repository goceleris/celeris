//go:build redis

package redis_test

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// dialNet opens a TCP connection with the given deadline. It is used by
// pubsub/auth tests that need to speak RESP directly (PUBLISH, CLIENT KILL)
// without going through the driver's command pool.
func dialNet(addr string, deadline time.Time) (net.Conn, error) {
	d := net.Dialer{Deadline: deadline}
	return d.Dial("tcp", addr)
}

// encodeCommand produces a RESP2 multi-bulk command string.
func encodeCommand(args ...string) []byte {
	var sb strings.Builder
	sb.WriteByte('*')
	sb.WriteString(strconv.Itoa(len(args)))
	sb.WriteString("\r\n")
	for _, a := range args {
		sb.WriteByte('$')
		sb.WriteString(strconv.Itoa(len(a)))
		sb.WriteString("\r\n")
		sb.WriteString(a)
		sb.WriteString("\r\n")
	}
	return []byte(sb.String())
}

// readLine reads one RESP line (up to \r\n) and returns the payload sans CRLF.
// For replies that are followed by additional frames (e.g. bulk strings), the
// caller must drain the rest — this helper is only used for simple/error/int
// replies.
func readLine(conn net.Conn) (string, error) {
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 2 {
		return "", fmt.Errorf("short line: %q", line)
	}
	return line[:len(line)-2], nil
}
