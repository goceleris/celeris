//go:build unix

package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// dialRedisConn dials a TCP connection, sets it non-blocking, registers it
// with the event loop, and runs the HELLO handshake.
func dialRedisConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*redisConn, error) {
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

	file, err := tcp.File()
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	fd := int(file.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = tcp.Close()
		_ = file.Close()
		return nil, err
	}
	tuneConnSocket(fd)
	_ = tcp.Close()

	nw := prov.NumWorkers()
	if nw <= 0 {
		// Close via file.Close() so the *os.File's runtime finalizer is
		// disarmed. syscall.Close(fd) alone would leave the finalizer
		// armed; GC could then call syscall.Close(fd) a second time on an
		// fd the kernel has since reassigned to another socket.
		_ = file.Close()
		return nil, errors.New("celeris-redis: event loop has 0 workers")
	}
	if workerHint < 0 || workerHint >= nw {
		workerHint = fd % nw
		if workerHint < 0 {
			workerHint = -workerHint
		}
	}
	loop := prov.WorkerLoop(workerHint)

	c := &redisConn{
		fd:        fd,
		loop:      loop,
		workerID:  workerHint,
		file:      file,
		state:     newRedisState(),
		cfg:       cfg,
		createdAt: time.Now(),
	}
	// Cache SyncRoundTripper capability for the fast path.
	if srt, ok := loop.(syncRoundTripper); ok {
		c.sync = srt
	}
	if sbrt, ok := loop.(syncBusyRoundTripper); ok {
		c.syncBusy = sbrt
	}
	// When the driver was opened WithEngine, the handler that calls into
	// us runs on the celeris HTTP engine's LockOSThread'd worker. Route
	// through the busy variant to avoid Gosched's locked-M futex storm.
	c.useBusy = cfg.Engine != nil && c.syncBusy != nil
	if smrt, ok := loop.(syncMultiRoundTripper); ok {
		c.syncMulti = smrt
	}
	c.onRecvFn = c.onRecv
	c.onCloseFn = c.onClose
	c.pollIsDoneFn = c.pollIsDone
	c.pollBeforeRearmFn = c.pollBeforeRearm
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := loop.RegisterConn(fd, c.onRecvFn, c.onCloseFn); err != nil {
		// Close via file.Close() to disarm the *os.File finalizer; a plain
		// syscall.Close(fd) would leak the finalizer and risk a double
		// close on a reassigned fd when GC later runs it.
		_ = file.Close()
		return nil, err
	}

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
