// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// muxCounter is a drpcmetrics.Counter backed by an atomic so the test can read
// it from the test goroutine while the manager increments from its own.
type muxCounter struct{ n *atomic.Int64 }

func (c muxCounter) Inc(v int64) { c.n.Add(v) }

// muxGauge is a drpcmetrics.Gauge backed by an atomic so the test can read the
// latest value set by the manager from another goroutine.
type muxGauge struct{ n *atomic.Int64 }

func (g muxGauge) Update(v int64) { g.n.Store(v) }

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
	opened, closed, failed, recvBlocked, writeBlocked atomic.Int64
}

func (m *muxCounters) bundle(shouldRecord func() bool) *drpcmetrics.MuxMetrics {
	return &drpcmetrics.MuxMetrics{
		StreamsOpened: muxCounter{&m.opened},
		StreamsClosed: muxCounter{&m.closed},
		StreamsFailed: muxCounter{&m.failed},
		RecvBlocked:   muxCounter{&m.recvBlocked},
		Blocked:       muxGauge{&m.writeBlocked},
		ShouldRecord:  shouldRecord,
	}
}

// waitForCount polls n until it reaches target or the deadline expires.
func waitForCount(t *testing.T, n *atomic.Int64, target int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < target {
		if time.Now().After(deadline) {
			t.Fatalf("counter reached %d, want %d", n.Load(), target)
		}
		time.Sleep(time.Millisecond)
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

// TestManagerRecvBlocked verifies the end-to-end receive-block path: a stream
// whose consumer never reads fills its receive buffer, stalls the transport
// reader, and increments RecvBlocked exactly once.
func TestManagerRecvBlocked(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	var c muxCounters
	cman := NewWithOptions(cconn, Client, Options{
		MuxMetrics: c.bundle(func() bool { return true }),
	})
	defer func() { _ = cman.Close() }()

	// Open a client stream and never read from it, so incoming messages pile up
	// in its receive buffer.
	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)

	// Stream messages at the stream until the reader stalls. We send one frame
	// per Write and keep going; once the receive buffer is full the reader
	// parks inside Enqueue (firing the hook) and stops consuming, so further
	// Writes block and the loop exits when the connection is closed.
	ctx.Run(func(context.Context) {
		for mid := uint64(1); ; mid++ {
			var buf []byte
			buf = drpcwire.AppendFrame(buf, createFrame(drpcwire.KindMessage, stream.ID(), mid, "x", true))
			if _, err := sconn.Write(buf); err != nil {
				return
			}
		}
	})

	// The hook fires exactly once: when the buffer first becomes full. The
	// reader is then parked and enqueues nothing more, so the count stays at 1.
	waitForCount(t, &c.recvBlocked, 1)
	assert.Equal(t, c.recvBlocked.Load(), int64(1))
}

// TestManagerRecvBlockedGated verifies that the receive-block hook honors the
// ShouldRecord gate: it records only when gating is on.
func TestManagerRecvBlockedGated(t *testing.T) {
	newManager := func(t *testing.T, shouldRecord func() bool) (*Manager, *muxCounters) {
		t.Helper()
		cconn, sconn := net.Pipe()
		t.Cleanup(func() { _ = cconn.Close(); _ = sconn.Close() })
		c := &muxCounters{}
		m := NewWithOptions(cconn, Client, Options{MuxMetrics: c.bundle(shouldRecord)})
		t.Cleanup(func() { _ = m.Close() })
		return m, c
	}

	off, offC := newManager(t, func() bool { return false })
	off.onRecvBlock()
	assert.Equal(t, offC.recvBlocked.Load(), int64(0))

	on, onC := newManager(t, func() bool { return true })
	on.onRecvBlock()
	assert.Equal(t, onC.recvBlocked.Load(), int64(1))
}

// TestManagerWriteBlocked verifies the end-to-end write-backpressure path: with
// a tiny write buffer and a transport whose writes never drain (no reader on
// the other end), a stream writer parks on backpressure and the Blocked gauge
// rises to one.
func TestManagerWriteBlocked(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	// Intentionally never read from sconn: the writer's run goroutine stalls on
	// the first flush, so the pending buffer fills and producers park.

	var c muxCounters
	cman := NewWithOptions(cconn, Client, Options{
		Writer:     drpcwire.WriterOptions{MaximumBufferSize: 1},
		MuxMetrics: c.bundle(func() bool { return true }),
	})
	defer func() { _ = cman.Close() }()

	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)

	// Keep writing; with the writer stalled and a 1-byte high-water mark, a
	// writer soon parks on backpressure. RawWrite returns once the manager is
	// closed during cleanup.
	ctx.Run(func(context.Context) {
		for {
			if err := stream.RawWrite(drpcwire.KindMessage, []byte("x")); err != nil {
				return
			}
		}
	})

	waitForCount(t, &c.writeBlocked, 1)
}

// TestManagerWriteBlockedGatedOff verifies that with gating off the Blocked
// gauge is never touched, even though a writer does park on backpressure.
func TestManagerWriteBlockedGatedOff(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	var c muxCounters
	cman := NewWithOptions(cconn, Client, Options{
		Writer:     drpcwire.WriterOptions{MaximumBufferSize: 1},
		MuxMetrics: c.bundle(func() bool { return false }),
	})
	defer func() { _ = cman.Close() }()

	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)

	ctx.Run(func(context.Context) {
		for {
			if err := stream.RawWrite(drpcwire.KindMessage, []byte("x")); err != nil {
				return
			}
		}
	})

	// A writer parks within milliseconds (the transport never drains), but with
	// gating off the gauge must remain zero. Give it ample time to park.
	time.Sleep(250 * time.Millisecond)
	assert.Equal(t, c.writeBlocked.Load(), int64(0))
}
