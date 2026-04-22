// Package services owns the lifecycle of the external datastores that the
// perfmatrix driver scenarios talk to (Postgres, Redis, Memcached). Each
// service is started in a container, probed until ready, seeded with a
// known fixture set, and torn down when the orchestrator exits.
//
// The concrete container-management implementation is filled in by
// wave-2; this scaffold only pins the public contract.
package services

import (
	"context"
	"errors"
)

// Kind enumerates the services perfmatrix can provision. String values are
// accepted by Start to select which subset to bring up.
const (
	KindPostgres  = "postgres"
	KindRedis     = "redis"
	KindMemcached = "memcached"
)

// Handles is the set of running services returned by Start. Fields are nil
// when the corresponding service was not requested. Driver scenarios read
// DSNs/addresses off the Handles to configure their clients.
type Handles struct {
	Postgres  *PGService
	Redis     *RedisService
	Memcached *MCService
}

// PGService describes a running Postgres instance.
type PGService struct {
	// DSN is a libpq connection string, e.g.
	// "postgres://user:pw@127.0.0.1:5432/perfmatrix?sslmode=disable".
	DSN string
}

// RedisService describes a running Redis instance.
type RedisService struct {
	// Addr is "host:port".
	Addr string
}

// MCService describes a running Memcached instance.
type MCService struct {
	// Addr is "host:port".
	Addr string
}

// ErrNotImplemented is returned by every scaffold stub in this package.
// Wave-2 replaces these stubs with real container plumbing.
var ErrNotImplemented = errors.New("perfmatrix/services: not yet implemented")

// Start provisions every service named in kinds (see the Kind* constants).
// It waits until each service is accepting connections before returning.
// Pass zero kinds to skip provisioning entirely; the returned Handles will
// have all fields nil.
func Start(ctx context.Context, kinds ...string) (*Handles, error) {
	_ = ctx
	_ = kinds
	return nil, ErrNotImplemented
}

// Stop tears down every provisioned service. It is safe to call with a nil
// receiver (no-op) so orchestrator cleanup paths need not branch.
func (h *Handles) Stop(ctx context.Context) error {
	if h == nil {
		return nil
	}
	_ = ctx
	return ErrNotImplemented
}

// Seed loads the known fixture set into every provisioned service:
// Postgres user id=42, Redis key "demo", Memcached key "demo", session
// cookie fixtures, etc. Driver scenarios depend on these fixtures being
// present. It is safe to call with a nil receiver (no-op).
func (h *Handles) Seed(ctx context.Context) error {
	if h == nil {
		return nil
	}
	_ = ctx
	return ErrNotImplemented
}
