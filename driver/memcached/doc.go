// Package memcached is celeris's native Memcached driver, speaking the text
// and binary wire protocols directly on the celeris event loop.
//
// [NewClient] returns a [Client], a pooled single-node handle whose typed API
// covers storage (Set, Add, Replace, Append, Prepend, CAS and their *Bytes
// variants), retrieval (Get, GetBytes, GetMulti, GetMultiBytes, Gets for the
// CAS token), arithmetic (Incr, Decr), key ops (Delete, Touch), and server
// ops (Flush, FlushAfter, Stats, Version, Ping). [ClusterClient], built with
// [NewClusterClient], mirrors the same command surface but shards keys across
// several nodes via a libmemcached-compatible ketama ring with passive failure
// detection and background health probing.
//
// Behavior is tuned with [Option] values passed to [NewClient]: [WithProtocol]
// ([ProtocolText] default, [ProtocolBinary] opt-in), [WithEngine] to colocate
// socket I/O on a celeris.Server's worker threads, [WithMaxOpen],
// [WithMaxIdlePerWorker], [WithTimeout], [WithDialTimeout], [WithMaxLifetime],
// [WithMaxIdleTime], and [WithHealthCheckInterval]. Addresses may carry an
// optional "memcache://" or "memcached://" scheme prefix.
//
// Misses and precondition failures surface as the sentinels [ErrCacheMiss],
// [ErrNotStored], and [ErrCASConflict]; other client-side faults as
// [ErrClosed], [ErrProtocol], [ErrPoolExhausted], [ErrMalformedKey],
// [ErrInvalidCAS], and (for clusters) [ErrNoNodes]. Other server-side replies arrive as [*MemcachedError].
//
// TLS, SASL authentication, and server-side compression are not supported;
// deploy over a trusted network and compress large values client-side.
// [ClusterClient] topology is static — to add or remove nodes, rebuild the
// client or front the fleet with a proxy such as mcrouter.
//
//	client, err := memcached.NewClient("localhost:11211")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//	if err := client.Set(ctx, "key", "value", time.Minute); err != nil {
//	    log.Fatal(err)
//	}
//	v, err := client.Get(ctx, "key")
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
package memcached
