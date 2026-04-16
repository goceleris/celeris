package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/redis/protocol"
)

// SentinelConfig configures a Sentinel-managed Redis client.
type SentinelConfig struct {
	// MasterName is the Sentinel master group name (required).
	MasterName string
	// SentinelAddrs is the list of sentinel addresses in "host:port" form.
	// At least one is required.
	SentinelAddrs []string
	// Password is the AUTH credential for the Redis master (not sentinel).
	Password string
	// SentinelPassword is the AUTH credential for sentinel connections (Redis 6+).
	SentinelPassword string
	// Username is the ACL user for the Redis master (Redis 6+).
	Username string
	// DB is the database index applied via SELECT on the master.
	DB int

	PoolSize         int
	MaxIdlePerWorker int
	DialTimeout      time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	Engine           eventloop.ServerProvider
}

// SentinelClient is a Redis client managed by Redis Sentinel. It discovers the
// current primary via Sentinel, connects to it, and automatically reconnects
// to the new primary when a failover is detected.
type SentinelClient struct {
	cfg    SentinelConfig
	client *Client
	mu     sync.RWMutex

	sentinelMu    sync.Mutex
	sentinelConns []*sentinelConn

	closeCh chan struct{}
	closed  atomic.Bool
}

type sentinelConn struct {
	addr   string
	client *Client
	subPS  *PubSub
}

// NewSentinelClient creates a Sentinel-managed client. It connects to the
// first responsive sentinel, discovers the master address, and dials the
// master. A background goroutine subscribes to +switch-master on sentinel
// connections for automatic failover handling.
func NewSentinelClient(cfg SentinelConfig) (*SentinelClient, error) {
	if cfg.MasterName == "" {
		return nil, errors.New("celeris/redis: SentinelConfig.MasterName is required")
	}
	if len(cfg.SentinelAddrs) == 0 {
		return nil, errors.New("celeris/redis: SentinelConfig.SentinelAddrs requires at least one address")
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	s := &SentinelClient{
		cfg:     cfg,
		closeCh: make(chan struct{}),
	}
	masterAddr, err := s.discoverMaster()
	if err != nil {
		return nil, fmt.Errorf("celeris/redis: sentinel master discovery failed: %w", err)
	}
	client, err := s.dialMaster(masterAddr)
	if err != nil {
		return nil, fmt.Errorf("celeris/redis: dial master %s failed: %w", masterAddr, err)
	}
	s.client = client
	go s.subscribeLoop()
	return s, nil
}

// Close tears down the sentinel client and all sentinel connections.
func (s *SentinelClient) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.closeCh)
	s.sentinelMu.Lock()
	for _, sc := range s.sentinelConns {
		if sc.subPS != nil {
			_ = sc.subPS.Close()
		}
		if sc.client != nil {
			_ = sc.client.Close()
		}
	}
	s.sentinelConns = nil
	s.sentinelMu.Unlock()

	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// getClient returns the current master client under read lock.
func (s *SentinelClient) getClient() (*Client, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return nil, ErrSentinelUnhealthy
	}
	return c, nil
}

// discoverMaster queries sentinels for the master address.
func (s *SentinelClient) discoverMaster() (string, error) {
	for _, addr := range s.cfg.SentinelAddrs {
		masterAddr, err := s.queryMaster(addr)
		if err != nil {
			continue
		}
		return masterAddr, nil
	}
	return "", fmt.Errorf("no responsive sentinel for master %q", s.cfg.MasterName)
}

// queryMaster connects to a single sentinel and asks for the master address.
func (s *SentinelClient) queryMaster(sentinelAddr string) (string, error) {
	var opts []Option
	opts = append(opts, WithDialTimeout(s.cfg.DialTimeout))
	if s.cfg.SentinelPassword != "" {
		opts = append(opts, WithPassword(s.cfg.SentinelPassword))
	}
	if s.cfg.Engine != nil {
		opts = append(opts, WithEngine(s.cfg.Engine))
	}
	opts = append(opts, WithForceRESP2())
	c, err := NewClient(sentinelAddr, opts...)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()

	v, err := c.Do(context.Background(), "SENTINEL", "get-master-addr-by-name", s.cfg.MasterName)
	if err != nil {
		return "", err
	}
	if v.Type == protocol.TyNull {
		return "", fmt.Errorf("sentinel %s: master %q not found", sentinelAddr, s.cfg.MasterName)
	}
	if v.Type != protocol.TyArray || len(v.Array) < 2 {
		return "", fmt.Errorf("sentinel %s: unexpected reply type %s", sentinelAddr, v.Type)
	}
	host := string(v.Array[0].Str)
	port := string(v.Array[1].Str)
	return net.JoinHostPort(host, port), nil
}

// dialMaster creates a Client to the given master address, then verifies the
// connection is actually a master using the ROLE command.
func (s *SentinelClient) dialMaster(addr string) (*Client, error) {
	var opts []Option
	if s.cfg.Password != "" {
		opts = append(opts, WithPassword(s.cfg.Password))
	}
	if s.cfg.Username != "" {
		opts = append(opts, WithUsername(s.cfg.Username))
	}
	if s.cfg.DB != 0 {
		opts = append(opts, WithDB(s.cfg.DB))
	}
	if s.cfg.DialTimeout > 0 {
		opts = append(opts, WithDialTimeout(s.cfg.DialTimeout))
	}
	if s.cfg.ReadTimeout > 0 {
		opts = append(opts, WithReadTimeout(s.cfg.ReadTimeout))
	}
	if s.cfg.WriteTimeout > 0 {
		opts = append(opts, WithWriteTimeout(s.cfg.WriteTimeout))
	}
	if s.cfg.PoolSize > 0 {
		opts = append(opts, WithPoolSize(s.cfg.PoolSize))
	}
	if s.cfg.MaxIdlePerWorker > 0 {
		opts = append(opts, WithMaxIdlePerWorker(s.cfg.MaxIdlePerWorker))
	}
	if s.cfg.Engine != nil {
		opts = append(opts, WithEngine(s.cfg.Engine))
	}
	c, err := NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DialTimeout)
	defer cancel()
	v, err := c.Do(ctx, "ROLE")
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ROLE failed: %w", err)
	}
	if v.Type != protocol.TyArray || len(v.Array) < 1 {
		_ = c.Close()
		return nil, fmt.Errorf("unexpected ROLE reply type %s", v.Type)
	}
	role := string(v.Array[0].Str)
	if role != "master" {
		_ = c.Close()
		return nil, fmt.Errorf("node %s has role %q, expected master", addr, role)
	}
	return c, nil
}

// subscribeLoop subscribes to +switch-master on sentinel connections and
// handles failovers. It reconnects to sentinels with backoff when they
// disconnect.
func (s *SentinelClient) subscribeLoop() {
	bo := async.NewBackoff(100*time.Millisecond, 10*time.Second)
	attempt := 0
	for {
		if s.closed.Load() {
			return
		}
		sc, err := s.connectSentinel()
		if err != nil {
			delay := bo.Next(attempt)
			attempt++
			select {
			case <-time.After(delay):
			case <-s.closeCh:
				return
			}
			continue
		}
		bo.Reset()
		attempt = 0
		s.sentinelMu.Lock()
		s.sentinelConns = append(s.sentinelConns, sc)
		s.sentinelMu.Unlock()
		s.watchFailover(sc)
	}
}

// connectSentinel connects to the first responsive sentinel and subscribes to
// the +switch-master channel.
func (s *SentinelClient) connectSentinel() (*sentinelConn, error) {
	for _, addr := range s.cfg.SentinelAddrs {
		var opts []Option
		opts = append(opts, WithDialTimeout(s.cfg.DialTimeout))
		if s.cfg.SentinelPassword != "" {
			opts = append(opts, WithPassword(s.cfg.SentinelPassword))
		}
		if s.cfg.Engine != nil {
			opts = append(opts, WithEngine(s.cfg.Engine))
		}
		opts = append(opts, WithForceRESP2())
		c, err := NewClient(addr, opts...)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DialTimeout)
		ps, err := c.Subscribe(ctx, "+switch-master")
		cancel()
		if err != nil {
			_ = c.Close()
			continue
		}
		return &sentinelConn{addr: addr, client: c, subPS: ps}, nil
	}
	return nil, errors.New("no responsive sentinel")
}

// watchFailover reads +switch-master messages and triggers a master swap.
func (s *SentinelClient) watchFailover(sc *sentinelConn) {
	ch := sc.subPS.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if msg == nil {
				continue
			}
			// +switch-master payload: "mymaster old-ip old-port new-ip new-port"
			s.handleSwitchMaster(msg.Payload)
		case <-s.closeCh:
			return
		}
	}
}

// ErrSentinelUnhealthy is returned by commands when the sentinel client
// failed to connect to the new master after a failover and is in an
// unhealthy state.
var ErrSentinelUnhealthy = errors.New("celeris/redis: sentinel client unhealthy, failover dial failed")

// handleSwitchMaster parses the sentinel notification and swaps the master client.
// On dial failure, it retries 3 times with exponential backoff. If all retries
// fail, the client is marked unhealthy and returns errors until the next
// successful +switch-master.
func (s *SentinelClient) handleSwitchMaster(payload string) {
	// Format: "<master-name> <old-ip> <old-port> <new-ip> <new-port>"
	parts := splitFields(payload)
	if len(parts) < 5 {
		return
	}
	if parts[0] != s.cfg.MasterName {
		return
	}
	newAddr := net.JoinHostPort(parts[3], parts[4])

	bo := async.NewBackoff(100*time.Millisecond, 2*time.Second)
	const maxRetries = 3
	var newClient *Client
	var lastErr error
	for attempt := range maxRetries {
		var err error
		newClient, err = s.dialMaster(newAddr)
		if err == nil {
			break
		}
		lastErr = err
		newClient = nil
		if attempt < maxRetries-1 {
			delay := bo.Next(attempt)
			select {
			case <-time.After(delay):
			case <-s.closeCh:
				return
			}
		}
	}

	s.mu.Lock()
	old := s.client
	if newClient != nil {
		s.client = newClient
	} else {
		// Mark unhealthy: nil out client so subsequent commands fail with
		// a clear error instead of silently using the old (now-replica) conn.
		s.client = nil
		_ = lastErr // recorded for diagnostics; callers get ErrSentinelUnhealthy
	}
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// splitFields splits a string on whitespace.
func splitFields(s string) []string {
	var fields []string
	start := -1
	for i, c := range s {
		if c == ' ' || c == '\t' {
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
		} else {
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}

// ---------- Delegated Client methods ----------

func (s *SentinelClient) Ping(ctx context.Context) error {
	c, err := s.getClient()
	if err != nil {
		return err
	}
	return c.Ping(ctx)
}

func (s *SentinelClient) Get(ctx context.Context, key string) (string, error) {
	c, err := s.getClient()
	if err != nil {
		return "", err
	}
	return c.Get(ctx, key)
}

func (s *SentinelClient) GetBytes(ctx context.Context, key string) ([]byte, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.GetBytes(ctx, key)
}

func (s *SentinelClient) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	c, err := s.getClient()
	if err != nil {
		return err
	}
	return c.Set(ctx, key, value, expiration)
}

func (s *SentinelClient) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	c, err := s.getClient()
	if err != nil {
		return false, err
	}
	return c.SetNX(ctx, key, value, expiration)
}

func (s *SentinelClient) Del(ctx context.Context, keys ...string) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.Del(ctx, keys...)
}

func (s *SentinelClient) Exists(ctx context.Context, keys ...string) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.Exists(ctx, keys...)
}

func (s *SentinelClient) Incr(ctx context.Context, key string) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.Incr(ctx, key)
}

func (s *SentinelClient) Decr(ctx context.Context, key string) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.Decr(ctx, key)
}

func (s *SentinelClient) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	c, err := s.getClient()
	if err != nil {
		return false, err
	}
	return c.Expire(ctx, key, expiration)
}

func (s *SentinelClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.TTL(ctx, key)
}

func (s *SentinelClient) MGet(ctx context.Context, keys ...string) ([]string, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.MGet(ctx, keys...)
}

func (s *SentinelClient) MSet(ctx context.Context, pairs ...any) error {
	c, err := s.getClient()
	if err != nil {
		return err
	}
	return c.MSet(ctx, pairs...)
}

func (s *SentinelClient) HGet(ctx context.Context, key, field string) (string, error) {
	c, err := s.getClient()
	if err != nil {
		return "", err
	}
	return c.HGet(ctx, key, field)
}

func (s *SentinelClient) HSet(ctx context.Context, key string, values ...any) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.HSet(ctx, key, values...)
}

func (s *SentinelClient) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.HDel(ctx, key, fields...)
}

func (s *SentinelClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.HGetAll(ctx, key)
}

func (s *SentinelClient) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.LPush(ctx, key, values...)
}

func (s *SentinelClient) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.RPush(ctx, key, values...)
}

func (s *SentinelClient) LPop(ctx context.Context, key string) (string, error) {
	c, err := s.getClient()
	if err != nil {
		return "", err
	}
	return c.LPop(ctx, key)
}

func (s *SentinelClient) RPop(ctx context.Context, key string) (string, error) {
	c, err := s.getClient()
	if err != nil {
		return "", err
	}
	return c.RPop(ctx, key)
}

func (s *SentinelClient) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.LRange(ctx, key, start, stop)
}

func (s *SentinelClient) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.SAdd(ctx, key, members...)
}

func (s *SentinelClient) SMembers(ctx context.Context, key string) ([]string, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.SMembers(ctx, key)
}

func (s *SentinelClient) ZAdd(ctx context.Context, key string, members ...Z) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.ZAdd(ctx, key, members...)
}

func (s *SentinelClient) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.ZRange(ctx, key, start, stop)
}

func (s *SentinelClient) Publish(ctx context.Context, channel, message string) (int64, error) {
	c, err := s.getClient()
	if err != nil {
		return 0, err
	}
	return c.Publish(ctx, channel, message)
}

func (s *SentinelClient) Subscribe(ctx context.Context, channels ...string) (*PubSub, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.Subscribe(ctx, channels...)
}

func (s *SentinelClient) Do(ctx context.Context, args ...any) (*protocol.Value, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, args...)
}

func (s *SentinelClient) Pipeline() *Pipeline {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return nil
	}
	return c.Pipeline()
}

func (s *SentinelClient) Stats() async.PoolStats {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return async.PoolStats{}
	}
	return c.Stats()
}
