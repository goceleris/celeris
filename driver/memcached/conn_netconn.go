package memcached

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// dialMemcachedConnNetConn is dialMemcachedConn for a loop that adopts a
// net.Conn instead of a file descriptor (eventloop.NetConnRegistrar): the
// transport of platforms without unix descriptors (windows), where the
// portable goroutine-per-connection loop reads the conn and calls onRecv. It
// is compiled everywhere so that its tests run on Linux CI as well; only
// conn_other.go selects it as dialMemcachedConn.
//
// The conn has no descriptor: c.fd holds the identifier the loop assigned,
// c.file stays nil, and the loop closes the net.Conn on UnregisterConn, which
// Close runs.
func dialMemcachedConnNetConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*memcachedConn, error) {
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
		return nil, fmt.Errorf("celeris-memcached: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	nw := prov.NumWorkers()
	if nw <= 0 {
		_ = tcp.Close()
		return nil, errors.New("celeris-memcached: event loop has 0 workers")
	}
	if workerHint < 0 || workerHint >= nw {
		workerHint = 0
	}
	loop := prov.WorkerLoop(workerHint)
	reg, ok := loop.(eventloop.NetConnRegistrar)
	if !ok {
		_ = tcp.Close()
		return nil, fmt.Errorf("celeris-memcached: event loop worker %T cannot adopt a net.Conn", loop)
	}

	bin := cfg.Protocol == ProtocolBinary
	c := &memcachedConn{
		loop:      loop,
		workerID:  workerHint,
		state:     newMCState(bin),
		cfg:       cfg,
		binary:    bin,
		createdAt: time.Now(),
	}
	c.onRecvFn = c.onRecv
	c.onCloseFn = c.onClose
	c.pollIsDoneFn = c.pollIsDone
	c.lastUsedAt.Store(time.Now().UnixNano())

	id, err := reg.RegisterNetConn(tcp, c.onRecvFn, c.onCloseFn)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	c.fd = id

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
