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
	"github.com/goceleris/celeris/internal/driver/redis/protocol"
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
	// DialTimeout bounds each new connection's TCP connect and, separately,
	// its handshake; default 5s. See [Config].DialTimeout.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	Engine       ServerProvider
}

// sentinelDownGrace is how long watchFailover lets the PubSub of the sentinel
// it follows stay down (see PubSub.downFor) before it gives that sentinel up
// and lets subscribeLoop connect to the next address of SentinelAddrs
// (celeris#887 item 29). A sentinel that restarts within it keeps the
// subscription.
var sentinelDownGrace = 5 * time.Second

// sentinelSwitchBackoffBase is the first pause of handleSwitchMaster between
// two dials of the new master (it doubles, up to 2 s). A variable so a test can
// make the pause end at once.
var sentinelSwitchBackoffBase = 100 * time.Millisecond

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

	// downGrace is sentinelDownGrace as of construction; zero (a client built
	// without NewSentinelClient) means the default.
	downGrace time.Duration
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
		return nil, errors.New("celeris-redis: SentinelConfig.MasterName is required")
	}
	if len(cfg.SentinelAddrs) == 0 {
		return nil, errors.New("celeris-redis: SentinelConfig.SentinelAddrs requires at least one address")
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	s := &SentinelClient{
		cfg:       cfg,
		closeCh:   make(chan struct{}),
		downGrace: sentinelDownGrace,
	}
	masterAddr, err := s.discoverMaster()
	if err != nil {
		return nil, fmt.Errorf("celeris-redis: sentinel master discovery failed: %w", err)
	}
	client, err := s.dialMaster(masterAddr)
	if err != nil {
		return nil, fmt.Errorf("celeris-redis: dial master %s failed: %w", masterAddr, err)
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
	// start is the index of the SentinelAddrs entry connectSentinel tries
	// first: 0, and after a sentinel was given up the one after it, so that a
	// sentinel that accepts and hangs up does not take the subscription back
	// from the next address for ever (celeris#887 item 29).
	start := 0
	for {
		if s.closed.Load() {
			return
		}
		sc, err := s.connectSentinelFrom(start)
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
		// Replace any stale entries — on a flappy sentinel the
		// previous sc's client is already closed (watchFailover
		// exits on read error and we fall back to reconnect),
		// and append-without-remove would grow sentinelConns
		// unboundedly across reconnects.
		// Cap the history to avoid unbounded growth under flappy
		// sentinels. Keep only the most recent entry and discard
		// the rest; watchFailover for the older ones has already
		// exited on read error (that's what triggered this
		// reconnect iteration).
		s.sentinelMu.Lock()
		if s.closed.Load() {
			// Close ran between the check at the top of the loop and here: its
			// list was already cleared, so it will never close sc. Whoever
			// takes a sentinel out of the list, or declines to put it there,
			// closes it (celeris#887 item 29).
			s.sentinelMu.Unlock()
			s.closeSentinel(sc)
			return
		}
		for _, old := range s.sentinelConns {
			// The PubSub first: its conn is checked out of the client's pool,
			// so closing the client does not stop its reconnect loop.
			if old != nil && old.subPS != nil {
				_ = old.subPS.Close()
			}
			if old != nil && old.client != nil {
				_ = old.client.Close()
			}
		}
		s.sentinelConns = append(s.sentinelConns[:0], sc)
		s.sentinelMu.Unlock()
		if s.watchFailover(sc) {
			start = s.sentinelIndex(sc.addr) + 1
		}
	}
}

// takeSentinel removes sc from sentinelConns and reports whether it was there.
// The caller that gets true owns sc's close.
func (s *SentinelClient) takeSentinel(sc *sentinelConn) bool {
	s.sentinelMu.Lock()
	defer s.sentinelMu.Unlock()
	for i, c := range s.sentinelConns {
		if c == sc {
			s.sentinelConns = append(s.sentinelConns[:i], s.sentinelConns[i+1:]...)
			return true
		}
	}
	return false
}

// closeSentinel closes sc's PubSub and then its client: the PubSub's conn is
// checked out of the client's pool, so closing the client alone does not stop
// its reconnect loop.
func (s *SentinelClient) closeSentinel(sc *sentinelConn) {
	if sc.subPS != nil {
		_ = sc.subPS.Close()
	}
	if sc.client != nil {
		_ = sc.client.Close()
	}
}

// sentinelIndex is the index of addr in SentinelAddrs, or -1.
func (s *SentinelClient) sentinelIndex(addr string) int {
	for i, a := range s.cfg.SentinelAddrs {
		if a == addr {
			return i
		}
	}
	return -1
}

// connectSentinel connects to the first responsive sentinel and subscribes to
// the +switch-master channel.
func (s *SentinelClient) connectSentinel() (*sentinelConn, error) {
	return s.connectSentinelFrom(0)
}

// connectSentinelFrom is connectSentinel trying the addresses of
// SentinelAddrs in order from index start, wrapping around once.
func (s *SentinelClient) connectSentinelFrom(start int) (*sentinelConn, error) {
	n := len(s.cfg.SentinelAddrs)
	for k := 0; k < n; k++ {
		addr := s.cfg.SentinelAddrs[((start+k)%n+n)%n]
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
		// Not Client.Subscribe: its constructor keeps the PubSub when the
		// first SUBSCRIBE finds the conn down (ErrPubSubConnDown, the reconnect
		// replays it). For a sentinel that is wrong: the reconnect would retry
		// this one address for ever while watchFailover waits, and no check
		// of the conn after the call can tell a conn that is down but not yet
		// marked closed (the loop forgot the descriptor, ErrUnknownFD) from a
		// live one. So the first SUBSCRIBE is made here and its own error
		// decides: any error, ErrPubSubConnDown included, skips this sentinel
		// as on main (celeris#887 item 12).
		ps, err := c.newPubSub(ctx)
		if err == nil {
			if err = ps.Subscribe(ctx, "+switch-master"); err != nil {
				_ = ps.Close()
			}
		}
		cancel()
		if err != nil {
			_ = c.Close()
			continue
		}
		return &sentinelConn{addr: addr, client: c, subPS: ps}, nil
	}
	return nil, errors.New("no responsive sentinel")
}

// watchFailover reads +switch-master messages and triggers a master swap. It
// returns true when it gave the sentinel up: the sentinel's PubSub reconnects
// to the same address for ever, so a sentinel that went away would otherwise
// keep the subscription and the failovers of the other sentinels would never
// be heard. The sentinel is given up once its PubSub has been without a
// reliable conn for sentinelDownGrace (PubSub.downFor: gone, or accepting and
// hanging up again and again); the PubSub and the client are closed, and
// subscribeLoop moves on to the next address of SentinelAddrs. It returns
// false when the SentinelClient is closed.
func (s *SentinelClient) watchFailover(sc *sentinelConn) (gaveUp bool) {
	ch := sc.subPS.Channel()
	grace := s.downGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	tick := time.NewTicker(max(grace/4, 5*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return !s.closed.Load()
			}
			if msg == nil {
				continue
			}
			// +switch-master payload: "mymaster old-ip old-port new-ip new-port"
			s.handleSwitchMaster(msg.Payload)
		case <-tick.C:
			if sc.subPS.downFor() >= grace {
				// Whoever takes sc out of sentinelConns closes it: if
				// SentinelClient.Close got there first it has closed (or is
				// closing, under sentinelMu) this very client, and a second
				// close would race it.
				if s.takeSentinel(sc) {
					s.closeSentinel(sc)
				}
				return !s.closed.Load()
			}
		case <-s.closeCh:
			return false
		}
	}
}

// ErrSentinelUnhealthy is returned by commands when the sentinel client
// failed to connect to the new master after a failover and is in an
// unhealthy state.
var ErrSentinelUnhealthy = errors.New("celeris-redis: sentinel client unhealthy, failover dial failed")

// handleSwitchMaster parses the sentinel notification and swaps the master client.
// On dial failure, it retries 3 times with exponential backoff. If all retries
// fail, the client is marked unhealthy and returns errors until the next
// successful +switch-master.
func (s *SentinelClient) handleSwitchMaster(payload string) {
	// watchFailover's select between a queued message and closeCh picks at
	// random, so this can run after Close: make no dial for a closed client.
	if s.closed.Load() {
		return
	}
	// Format: "<master-name> <old-ip> <old-port> <new-ip> <new-port>"
	parts := splitFields(payload)
	if len(parts) < 5 {
		return
	}
	if parts[0] != s.cfg.MasterName {
		return
	}
	newAddr := net.JoinHostPort(parts[3], parts[4])

	bo := async.NewBackoff(sentinelSwitchBackoffBase, 2*time.Second)
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
			// closeCh wins a tie with the timer: select picks at random when
			// both are ready, and another dial would connect to the master
			// after Close.
			if s.closed.Load() {
				return
			}
		}
	}

	s.mu.Lock()
	// Close sets closed before it takes s.mu to clear s.client, and a dial
	// runs under its own timeout, not closeCh, so a Close can land while
	// dialMaster is in flight. Nothing closes a client installed after it.
	if s.closed.Load() {
		s.mu.Unlock()
		if newClient != nil {
			_ = newClient.Close()
		}
		return
	}
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

// Subscribe is [Client.Subscribe] on the current master: it returns the
// PubSub with a nil error when the conn is found down on the first SUBSCRIBE,
// and the reconnect replays the channels. The reconnect dials the master the
// SentinelClient has at that moment, so after a failover the PubSub follows the
// new master instead of retrying the old one, once its conn to the old master
// has dropped (a conn that stays up, on a loop shared with another Client or
// WithEngine, is not moved). When the SentinelClient is closed, a PubSub whose
// conn is down (or goes down afterwards) gives up: Channel() is closed.
func (s *SentinelClient) Subscribe(ctx context.Context, channels ...string) (*PubSub, error) {
	c, err := s.getClient()
	if err != nil {
		return nil, err
	}
	ps, err := c.newPubSubRouted(ctx, sentinelPubSubRoute{s})
	if err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return ps, nil
	}
	return ps.firstControl(ps.Subscribe(ctx, channels...))
}

func (s *SentinelClient) Do(ctx context.Context, args ...any) (*Value, error) {
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

func (s *SentinelClient) Stats() PoolStats {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return PoolStats{}
	}
	return c.Stats()
}
