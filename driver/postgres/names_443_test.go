package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

// The engine sentinels the event loop returns are reachable as
// postgres.Err* (celeris#443): a caller can match them with errors.Is
// without importing the engine package.
func TestEngineSentinelsNamed443(t *testing.T) {
	if ErrQueueFull != engine.ErrQueueFull || ErrUnknownFD != engine.ErrUnknownFD {
		t.Fatal("ErrQueueFull/ErrUnknownFD are not the engine's sentinels")
	}
	if !errors.Is(fmt.Errorf("write: %w", engine.ErrQueueFull), ErrQueueFull) {
		t.Fatal("a wrapped engine.ErrQueueFull does not match ErrQueueFull")
	}

	// A real event loop: Write on a descriptor the worker never registered.
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	err = prov.WorkerLoop(0).Write(1<<30, []byte{'X', 0, 0, 0, 4})
	if !errors.Is(err, ErrUnknownFD) {
		t.Fatalf("Write on an unregistered fd: err = %v, want ErrUnknownFD", err)
	}
	if errors.Is(err, ErrQueueFull) {
		t.Fatalf("Write on an unregistered fd: err = %v matches ErrQueueFull too", err)
	}
}

// The codec's ErrInvalidLength reaches the caller unwrapped, so it is named
// here (celeris#443). The fake server answers the query with a message whose
// length header (2) is below the 4-byte minimum.
func TestInvalidLengthReachesCaller443(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			if typ, _, err := readMsg(c); err != nil || typ != protocol.MsgQuery {
				return
			}
			_, _ = c.Write([]byte{protocol.BackendCommandComplete, 0, 0, 0, 2})
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
	if err := c.Ping(ctx); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("Ping: err = %v, want ErrInvalidLength", err)
	}
}

// RegisterType and LookupOID act on the codec's registry, and TypeCodec is
// the codec's type.
func TestTypeCodecNames443(t *testing.T) {
	const oid = 0x7f44_3001 // no PostgreSQL type has this OID
	tc := &TypeCodec{
		OID:        oid,
		Name:       "m443_test",
		DecodeText: func(src []byte) (driver.Value, error) { return string(src), nil },
	}
	RegisterType(tc)
	if got := LookupOID(oid); got != tc {
		t.Fatalf("LookupOID = %p, want %p", got, tc)
	}
	if got := protocol.LookupOID(oid); got != tc {
		t.Fatalf("protocol.LookupOID = %p, want %p", got, tc)
	}
	if LookupOID(oid+1) != nil {
		t.Fatal("LookupOID of an unregistered OID is not nil")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterType(nil) did not panic")
		}
	}()
	RegisterType(nil)
}

// The date codec returns the infinity sentinels, which are named here
// (celeris#443).
func TestInfinityNamed443(t *testing.T) {
	c := LookupOID(protocol.OIDDate)
	for src, want := range map[string]time.Time{"infinity": PGInfinity, "-infinity": PGNegInfinity} {
		got, err := c.DecodeText([]byte(src))
		if err != nil || !got.(time.Time).Equal(want) {
			t.Fatalf("DecodeText(%q) = %v, %v; want %v", src, got, err, want)
		}
	}
	if !PGInfinity.Equal(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)) ||
		!PGNegInfinity.Equal(time.Date(-4713, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("PGInfinity/PGNegInfinity are not the documented values")
	}
}
