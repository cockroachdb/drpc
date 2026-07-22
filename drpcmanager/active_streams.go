// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"sync"

	"storj.io/drpc/drpcstream"
)

// activeStreams is a thread-safe map of stream IDs to stream objects.
// It is used by the Manager to track active streams for lifecycle management.
type activeStreams struct {
	mu         sync.RWMutex
	streams    map[uint64]*drpcstream.Stream
	closed     bool
	closeErr   error
	done       chan struct{}
	doneClosed bool
}

func newActiveStreams() *activeStreams {
	return &activeStreams{
		streams: make(map[uint64]*drpcstream.Stream),
		done:    make(chan struct{}),
	}
}

// Add adds a stream. It returns an error if the collection is closed or if a
// stream with the same ID already exists.
func (r *activeStreams) Add(id uint64, stream *drpcstream.Stream) error {
	if stream == nil {
		return managerClosed.New("stream can't be nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return r.closeErr
	}
	if _, ok := r.streams[id]; ok {
		return managerClosed.New("duplicate stream id")
	}
	r.streams[id] = stream
	return nil
}

// Remove removes a stream. If the collection is closed, removing the last
// stream unblocks Wait.
func (r *activeStreams) Remove(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.streams, id)
	r.signalDone()
}

// Get returns the stream for the given ID and whether it was found.
func (r *activeStreams) Get(id uint64) (*drpcstream.Stream, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, false
	}
	s, ok := r.streams[id]
	return s, ok
}

// Close cancels all active streams with the given error and marks the
// collection as closed to prevent future Add calls. Streams remain registered
// until their manage goroutines exit and remove them.
func (r *activeStreams) Close(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	r.closeErr = err
	for _, s := range r.streams {
		s.Cancel(err)
	}
	r.signalDone()
}

// Wait blocks until Close has been called and every registered stream has been
// removed.
func (r *activeStreams) Wait() {
	<-r.done
}

// signalDone closes done once shutdown has begun and no streams remain. The
// caller must hold r.mu.
func (r *activeStreams) signalDone() {
	if r.closed && len(r.streams) == 0 && !r.doneClosed {
		r.doneClosed = true
		close(r.done)
	}
}

// Len returns the number of active streams.
func (r *activeStreams) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.streams)
}
