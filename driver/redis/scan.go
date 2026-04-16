package redis

import (
	"context"
	"errors"
	"strconv"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// ScanIterator walks the keyspace via SCAN with automatic cursor paging. It
// is NOT safe for concurrent use; start one iterator per goroutine.
//
// Typical use:
//
//	it := client.Scan(ctx, "user:*", 100)
//	for {
//	    key, ok := it.Next(ctx)
//	    if !ok {
//	        break
//	    }
//	    // process key
//	}
//	if err := it.Err(); err != nil {
//	    // transport or server error
//	}
type ScanIterator struct {
	client *Client
	cursor string
	match  string
	count  int64

	batch []string
	idx   int
	err   error
	done  bool
}

// Scan returns a new iterator. match is the pattern passed as MATCH (empty
// means no MATCH arg). count is the COUNT hint (0 means no COUNT arg).
func (c *Client) Scan(ctx context.Context, match string, count int64) *ScanIterator {
	return &ScanIterator{
		client: c,
		cursor: "0",
		match:  match,
		count:  count,
	}
}

// Next returns the next key and true, or "" and false when the iteration
// ends or an error occurs. Call [ScanIterator.Err] to distinguish the two.
func (it *ScanIterator) Next(ctx context.Context) (string, bool) {
	if it.err != nil {
		return "", false
	}
	for it.idx >= len(it.batch) {
		if it.done {
			return "", false
		}
		if err := it.fetch(ctx); err != nil {
			it.err = err
			return "", false
		}
	}
	k := it.batch[it.idx]
	it.idx++
	return k, true
}

// Err returns any error encountered during iteration.
func (it *ScanIterator) Err() error { return it.err }

// fetch issues one SCAN call and refreshes the batch. Sets done when the
// server returns cursor "0".
func (it *ScanIterator) fetch(ctx context.Context) error {
	args := []string{"SCAN", it.cursor}
	if it.match != "" {
		args = append(args, "MATCH", it.match)
	}
	if it.count > 0 {
		args = append(args, "COUNT", strconv.FormatInt(it.count, 10))
	}
	var newCursor string
	var keys []string
	err := it.client.do(ctx, func(v protocol.Value) error {
		if v.Type != protocol.TyArray || len(v.Array) != 2 {
			return errors.New("celeris/redis: SCAN reply is not a 2-element array")
		}
		cur, e := asString(v.Array[0])
		if e != nil {
			return e
		}
		newCursor = cur
		ks, e := asStringSlice(v.Array[1])
		if e != nil {
			return e
		}
		keys = ks
		return nil
	}, args...)
	if err != nil {
		return err
	}
	it.cursor = newCursor
	it.batch = keys
	it.idx = 0
	if newCursor == "0" {
		it.done = true
	}
	return nil
}
