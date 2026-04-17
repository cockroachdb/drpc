// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import "sync"

// BufferPool wraps sync.Pool to provide reusable byte slices for the
// stream receive path. Buffers obtained via Get should be returned via
// Put when no longer needed. Forgetting to Put is safe (GC reclaims)
// but reduces reuse.
type BufferPool struct {
	pool sync.Pool
}

// NewBufferPool returns a new buffer pool.
func NewBufferPool() *BufferPool {
	return &BufferPool{
		pool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, 0, 4096)
				return &b
			},
		},
	}
}

// Get returns a zero-length byte slice from the pool, retaining its
// backing array for reuse.
func (bp *BufferPool) Get() *[]byte {
	p := bp.pool.Get().(*[]byte)
	*p = (*p)[:0]
	return p
}

// Put returns a buffer to the pool. Nil is safe to pass.
func (bp *BufferPool) Put(b *[]byte) {
	if b == nil {
		return
	}
	bp.pool.Put(b)
}
