// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// muxCounter is a drpcmetrics.Counter backed by an atomic so the test can read
// it from the test goroutine while the manager increments from its own.
type muxCounter struct{ n *atomic.Int64 }

func (c muxCounter) Inc(v int64) { c.n.Add(v) }

// drainConn reads and discards everything from c until it errors. net.Pipe is
// synchronous and unbuffered, so without a reader the manager's frame writes
// (invoke/message/close/cancel) would block.
func drainConn(ctx *drpctest.Tracker, c net.Conn) {
	ctx.Run(func(context.Context) {
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	})
}

type muxCounters struct {
	opened, closed, failed atomic.Int64
}

func (m *muxCounters) bundle(shouldRecord func() bool) *drpcmetrics.MuxMetrics {
	return &drpcmetrics.MuxMetrics{
		StreamsOpened: muxCounter{&m.opened},
		StreamsClosed: muxCounter{&m.closed},
		StreamsFailed: muxCounter{&m.failed},
		ShouldRecord:  shouldRecord,
	}
}

// TestManagerStreamsOpenedClosed verifies that opening a client stream
// increments StreamsOpened immediately and that a graceful local Close
// classifies the stream as closed (not failed).
func TestManagerStreamsOpenedClosed(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	drainConn(ctx, sconn)

	var c muxCounters
	cman := NewWithOptions(cconn, Client, Options{
		MuxMetrics: c.bundle(func() bool { return true }),
	})
	defer func() { _ = cman.Close() }()

	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)
	// StreamsOpened is incremented synchronously when the stream is created.
	assert.Equal(t, c.opened.Load(), int64(1))

	assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
	assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, []byte("hi")))
	assert.NoError(t, stream.Close())

	// Close waits for the manageStream goroutine (and its deferred outcome
	// classifier) to finish, so the close/failed counters are settled after
	// it returns.
	assert.NoError(t, cman.Close())
	assert.Equal(t, c.opened.Load(), int64(1))
	assert.Equal(t, c.closed.Load(), int64(1))
	assert.Equal(t, c.failed.Load(), int64(0))
}

// TestManagerStreamsFailed verifies that a stream torn down by context
// cancellation is classified as failed rather than closed.
func TestManagerStreamsFailed(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	drainConn(ctx, sconn)

	var c muxCounters
	cman := NewWithOptions(cconn, Client, Options{
		MuxMetrics: c.bundle(func() bool { return true }),
	})
	defer func() { _ = cman.Close() }()

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := cman.NewClientStream(streamCtx, "rpc")
	assert.NoError(t, err)
	assert.Equal(t, c.opened.Load(), int64(1))

	// Cancelling the stream's context drives manageStream down the cancel
	// path, terminating the stream with context.Canceled.
	cancel()
	<-stream.Finished()

	assert.NoError(t, cman.Close())
	assert.Equal(t, c.opened.Load(), int64(1))
	assert.Equal(t, c.failed.Load(), int64(1))
	assert.Equal(t, c.closed.Load(), int64(0))
}

// TestManagerStreamsGatedOff verifies that when ShouldRecord returns false, no
// handle is touched: neither the open nor the teardown classification records.
func TestManagerStreamsGatedOff(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	drainConn(ctx, sconn)

	var c muxCounters
	cman := NewWithOptions(cconn, Client, Options{
		MuxMetrics: c.bundle(func() bool { return false }),
	})
	defer func() { _ = cman.Close() }()

	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)
	assert.Equal(t, c.opened.Load(), int64(0))

	assert.NoError(t, stream.Close())
	assert.NoError(t, cman.Close())
	assert.Equal(t, c.opened.Load(), int64(0))
	assert.Equal(t, c.closed.Load(), int64(0))
	assert.Equal(t, c.failed.Load(), int64(0))
}
