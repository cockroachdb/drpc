// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcpool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zeebo/errs"

	"storj.io/drpc/drpcdebug"
)

// Options contains the options to configure a pool.
type Options struct {
	// Expiration will remove any idle values from the Pool after the
	// duration passes. Zero means no expiration.
	Expiration time.Duration

	// Capacity is the maximum number of values the Pool can store.
	// Zero means unlimited. Negative means no values.
	Capacity int

	// KeyCapacity is like Capacity except it is per key. Zero means
	// the Pool holds unlimited for any single key. Negative means
	// no values for any single key.
	KeyCapacity int

	// MaxStreamsPerConn is the maximum number of concurrent streams
	// allowed on a single pooled connection. Zero means unlimited.
	// Setting this to 1 gives legacy exclusive-access behavior.
	MaxStreamsPerConn int
}

// Pool is a connection pool with key type K. It maintains a set of connections
// per key and routes new streams to connections with available capacity.
// Connections stay in the pool while in use and track their active stream count.
type Pool[K comparable] struct {
	opts  Options
	mu    sync.Mutex
	byKey map[K][]*connState[K]
	all   []*connState[K]
}

// New constructs a new Pool with the provided Options.
func New[K comparable](opts Options) *Pool[K] {
	return &Pool[K]{
		opts:  opts,
		byKey: make(map[K][]*connState[K]),
	}
}

func (p *Pool[K]) log(what string, cb func() string) {
	if drpcdebug.Enabled {
		drpcdebug.Log(func() (_, _, _ string) { return fmt.Sprintf("<pül %p>", p), what, cb() })
	}
}

// Close evicts all entries from the Pool, closing them and returning all
// of the combined errors from closing.
func (p *Pool[K]) Close() (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var eg errs.Group
	for _, cs := range p.all {
		eg.Add(p.closeEntry(cs))
	}

	p.byKey = make(map[K][]*connState[K])
	p.all = nil

	return eg.Err()
}

// Get returns a new Conn that will use the provided dial function to create an
// underlying conn to be cached by the Pool when Conn methods are invoked. It
// will share any cached connections with other conns that use the same key.
func (p *Pool[K]) Get(ctx context.Context, key K,
	dial func(ctx context.Context, key K) (Conn, error)) Conn {
	return &poolConn[K]{
		key:  key,
		pool: p,
		dial: dial,
	}
}

func (p *Pool[K]) closeEntry(cs *connState[K]) error {
	p.log("CLOSE", cs.String)
	if cs.exp == nil || cs.exp.Stop() {
		return cs.val.Close()
	}
	return nil
}

// hasCapacity reports whether cs can accept another stream.
func (p *Pool[K]) hasCapacity(cs *connState[K]) bool {
	return p.opts.MaxStreamsPerConn == 0 || cs.active < p.opts.MaxStreamsPerConn
}

// acquire finds a connection for the given key that has available stream
// capacity and is not closed. Returns the connState and true if found.
func (p *Pool[K]) acquire(key K) (*connState[K], bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, cs := range p.byKey[key] {
		if closed(cs.val.Closed()) {
			continue
		}
		if !p.hasCapacity(cs) {
			continue
		}
		cs.active++
		if cs.exp != nil {
			cs.exp.Stop()
			cs.exp = nil
		}
		p.log("ACQUIRE", cs.String)
		return cs, true
	}

	return nil, false
}

// insertAndAcquire adds a newly dialed connection to the pool with active=1.
// It enforces capacity limits by evicting idle connections.
func (p *Pool[K]) insertAndAcquire(key K, val Conn) *connState[K] {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.opts.Capacity < 0 || p.opts.KeyCapacity < 0 {
		return &connState[K]{key: key, val: val, active: 1}
	}

	p.evictIdleLocked(key)

	cs := &connState[K]{key: key, val: val, active: 1}
	p.byKey[key] = append(p.byKey[key], cs)
	p.all = append(p.all, cs)
	p.log("INSERT+ACQUIRE", cs.String)
	return cs
}

// release decrements the active count for a connection. If the connection
// becomes idle and is closed, it is removed. Otherwise an expiration timer
// is started if configured.
func (p *Pool[K]) release(cs *connState[K]) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs.active--
	p.log("RELEASE", cs.String)

	if cs.active > 0 {
		return
	}

	if closed(cs.val.Closed()) {
		p.removeLocked(cs)
		return
	}

	p.evictExcessIdleLocked(cs)

	if p.opts.Expiration > 0 {
		cs.exp = time.AfterFunc(p.opts.Expiration, func() {
			_ = cs.val.Close()
			p.mu.Lock()
			p.removeLocked(cs)
			p.mu.Unlock()
		})
	}
}

// evictIdleLocked enforces KeyCapacity and Capacity limits by closing and
// removing idle (active == 0) connections. Must be called with p.mu held.
func (p *Pool[K]) evictIdleLocked(key K) {
	p.evictToLimitLocked(key, nil, 0)
}

// evictExcessIdleLocked evicts idle entries (other than keep) to bring
// the pool within KeyCapacity and Capacity limits.
// Must be called with p.mu held.
func (p *Pool[K]) evictExcessIdleLocked(keep *connState[K]) {
	p.evictToLimitLocked(keep.key, keep, 1)
}

// evictToLimitLocked evicts idle connections until KeyCapacity and Capacity
// limits are met. headroom is subtracted from each limit (0 = evict at limit,
// 1 = allow one over). skip is excluded from eviction (may be nil).
// Must be called with p.mu held.
func (p *Pool[K]) evictToLimitLocked(key K, skip *connState[K], headroom int) {
	if p.opts.KeyCapacity > 0 {
		for len(p.byKey[key]) > p.opts.KeyCapacity-1+headroom {
			if !p.evictOneIdleLocked(p.byKey[key], skip) {
				break
			}
		}
	}

	if p.opts.Capacity > 0 {
		for len(p.all) > p.opts.Capacity-1+headroom {
			if !p.evictOneIdleLocked(p.all, skip) {
				break
			}
		}
	}
}

// evictOneIdleLocked closes and removes the first idle entry in entries,
// skipping skip. Returns true if an entry was evicted.
func (p *Pool[K]) evictOneIdleLocked(entries []*connState[K], skip *connState[K]) bool {
	for _, cs := range entries {
		if cs != skip && cs.active == 0 {
			_ = p.closeEntry(cs)
			p.removeLocked(cs)
			return true
		}
	}
	return false
}

// removeLocked removes a connState from both byKey and all slices.
// Must be called with p.mu held.
func (p *Pool[K]) removeLocked(cs *connState[K]) {
	p.byKey[cs.key] = removeFromSlice(p.byKey[cs.key], cs)
	if len(p.byKey[cs.key]) == 0 {
		delete(p.byKey, cs.key)
	}
	p.all = removeFromSlice(p.all, cs)
}

func removeFromSlice[K comparable](s []*connState[K], cs *connState[K]) []*connState[K] {
	for i, c := range s {
		if c == cs {
			copy(s[i:], s[i+1:])
			s[len(s)-1] = nil
			return s[:len(s)-1]
		}
	}
	return s
}
