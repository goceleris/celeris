package store

import (
	"context"
	"time"
)

// Prefixed returns a [KV] that transparently prepends prefix to every key
// passed through Get/Set/Delete. Useful when sharing one backend among
// multiple middleware (e.g., session on "sess:" and cache on "cache:").
//
// Feature extensions (GetAndDeleter, Scanner, PrefixDeleter, SetNXer) on
// the inner KV are transparently surfaced: the returned value implements
// whichever of those the inner value implements, with keys/prefixes
// rewritten accordingly.
func Prefixed(inner KV, prefix string) KV {
	return &prefixedKV{inner: inner, prefix: prefix}
}

type prefixedKV struct {
	inner  KV
	prefix string
}

func (p *prefixedKV) Get(ctx context.Context, key string) ([]byte, error) {
	return p.inner.Get(ctx, p.prefix+key)
}

func (p *prefixedKV) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return p.inner.Set(ctx, p.prefix+key, value, ttl)
}

func (p *prefixedKV) Delete(ctx context.Context, key string) error {
	return p.inner.Delete(ctx, p.prefix+key)
}

func (p *prefixedKV) GetAndDelete(ctx context.Context, key string) ([]byte, error) {
	if gd, ok := p.inner.(GetAndDeleter); ok {
		return gd.GetAndDelete(ctx, p.prefix+key)
	}
	v, err := p.inner.Get(ctx, p.prefix+key)
	if err != nil {
		return nil, err
	}
	_ = p.inner.Delete(ctx, p.prefix+key)
	return v, nil
}

func (p *prefixedKV) Scan(ctx context.Context, prefix string) ([]string, error) {
	sc, ok := p.inner.(Scanner)
	if !ok {
		return nil, nil
	}
	full, err := sc.Scan(ctx, p.prefix+prefix)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(full))
	for _, k := range full {
		if len(k) >= len(p.prefix) {
			out = append(out, k[len(p.prefix):])
		}
	}
	return out, nil
}

func (p *prefixedKV) DeletePrefix(ctx context.Context, prefix string) error {
	if pd, ok := p.inner.(PrefixDeleter); ok {
		return pd.DeletePrefix(ctx, p.prefix+prefix)
	}
	sc, ok := p.inner.(Scanner)
	if !ok {
		return nil
	}
	keys, err := sc.Scan(ctx, p.prefix+prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		_ = p.inner.Delete(ctx, k)
	}
	return nil
}

func (p *prefixedKV) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	sn, ok := p.inner.(SetNXer)
	if !ok {
		v, err := p.inner.Get(ctx, p.prefix+key)
		if err == nil && v != nil {
			return false, nil
		}
		return true, p.inner.Set(ctx, p.prefix+key, value, ttl)
	}
	return sn.SetNX(ctx, p.prefix+key, value, ttl)
}
