// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"sync"

	"storj.io/drpc/drpcstream"
)

// streamRegistry is a thread-safe map of stream IDs to stream objects.
// It is used by the Manager to track all active streams for lifecycle
// management and packet routing.
type streamRegistry struct {
	mu      sync.RWMutex
	streams map[uint64]*drpcstream.Stream
	closed  bool
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{
		streams: make(map[uint64]*drpcstream.Stream),
	}
}

// Register adds a stream to the registry. It returns an error if the registry
// is closed or if a stream with the same ID is already registered.
func (r *streamRegistry) Register(id uint64, stream *drpcstream.Stream) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return managerClosed.New("register")
	}
	if _, ok := r.streams[id]; ok {
		return managerClosed.New("duplicate stream id")
	}
	r.streams[id] = stream
	return nil
}

// Unregister removes a stream from the registry. It is a no-op if the stream
// is not registered or if the registry has been closed.
func (r *streamRegistry) Unregister(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.streams != nil {
		delete(r.streams, id)
	}
}

// Get returns the stream for the given ID and whether it was found.
func (r *streamRegistry) Get(id uint64) (*drpcstream.Stream, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s, ok := r.streams[id]
	return s, ok
}

// Close marks the registry as closed, preventing future Register calls.
// It does not cancel any streams.
func (r *streamRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
}

// ForEach calls fn for each registered stream. The function is called with
// the stream ID and stream pointer. The registry is read-locked during iteration.
func (r *streamRegistry) ForEach(fn func(uint64, *drpcstream.Stream)) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for id, s := range r.streams {
		fn(id, s)
	}
}

// Len returns the number of registered streams.
func (r *streamRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.streams)
}
