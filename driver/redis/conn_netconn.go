package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// dialRedisConnNetConn is dialRedisConn for a loop that adopts a net.Conn
// instead of a file descriptor (eventloop.NetConnRegistrar): the transport of
// platforms without unix descriptors (windows), where the portable
// goroutine-per-connection loop reads the conn and calls onRecv. It is
// compiled everywhere so that its tests run on Linux CI as well; only
// conn_other.go selects it as dialRedisConn.
//
// The conn has no descriptor: c.fd holds the identifier the loop assigned
// (a number from eventloop.IsNetConnID's range, >= 1<<30, not a descriptor),
// c.file stays nil, and the loop closes the net.Conn on UnregisterConn, which
// Close runs. This function is compiled on unix too (its tests run there),
// where Close also reaches the raw-descriptor helpers of rawfd_unix.go with
// that identifier; they fail with EBADF and their result is ignored.
func dialRedisConnNetConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*redisConn, error) {
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("celeris-redis: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	nw := prov.NumWorkers()
	if nw <= 0 {
		_ = tcp.Close()
		return nil, errors.New("celeris-redis: event loop has 0 workers")
	}
	// The unix dial spreads conns over the workers by descriptor; a conn with
	// no descriptor has nothing to spread by, so an unusable hint (the
	// database/sql path passes -1) lands on worker 0. The portable worker
	// runs one goroutine per conn, so the shared map lock is the only cost.
	if workerHint < 0 || workerHint >= nw {
		workerHint = 0
	}
	loop := prov.WorkerLoop(workerHint)
	reg, ok := loop.(eventloop.NetConnRegistrar)
	if !ok {
		_ = tcp.Close()
		return nil, fmt.Errorf("celeris-redis: event loop worker %T cannot adopt a net.Conn", loop)
	}

	c := &redisConn{
		loop:      loop,
		workerID:  workerHint,
		state:     newRedisState(),
		cfg:       cfg,
		createdAt: time.Now(),
	}
	c.onRecvFn = c.onRecv
	c.onCloseFn = c.onClose
	c.pollIsDoneFn = c.pollIsDone
	c.pollBeforeRearmFn = c.pollBeforeRearm
	c.lastUsedAt.Store(time.Now().UnixNano())

	id, err := reg.RegisterNetConn(tcp, c.onRecvFn, c.onCloseFn)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	// c.fd is written after the reader goroutine may already be running, with
	// no synchronisation (the unix dial sets it before RegisterConn): no
	// callback may read c.fd, or this write becomes a race. (Postgres
	// publishes its identifier under fdMu because its onClose does read it.)
	c.fd = id

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
