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

// testCounter is a drpcmetrics.Counter backed by an atomic so the test can read
// it from the test goroutine while the manager increments from its own.
type testCounter struct{ n *atomic.Int64 }

func (c testCounter) Inc(v int64) { c.n.Add(v) }

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

type streamMetricCounters struct{ started, terminated atomic.Int64 }

func (m *streamMetricCounters) bundle() drpcmetrics.ConnectionMetrics {
	return drpcmetrics.ConnectionMetrics{
		StreamsStarted:    testCounter{&m.started},
		StreamsTerminated: testCounter{&m.terminated},
	}
}

// TestManagerStreamLifecycle verifies that a successfully admitted stream
// increments started, then increments terminated when its management goroutine
// finishes.
func TestManagerStreamLifecycle(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	drainConn(ctx, sconn)

	var c streamMetricCounters
	cman := NewWithOptions(cconn, Client, Options{
		Metrics:      c.bundle(),
		ShouldRecord: func() bool { return true },
	})
	defer func() { _ = cman.Close() }()

	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)
	assert.Equal(t, c.started.Load(), int64(1))
	assert.Equal(t, c.terminated.Load(), int64(0))

	assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
	assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, []byte("hi")))
	assert.NoError(t, stream.Close())

	// Manager.Close waits for manageStream, so all lifecycle updates are settled
	// when it returns.
	assert.NoError(t, cman.Close())
	assert.Equal(t, c.started.Load(), int64(1))
	assert.Equal(t, c.terminated.Load(), int64(1))
}

// TestManagerStreamLifecycleOnConnectionClose verifies that connection teardown
// terminates every active stream without needing to classify why it ended.
func TestManagerStreamLifecycleOnConnectionClose(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	var c streamMetricCounters
	cman := NewWithOptions(cconn, Client, Options{
		Metrics:      c.bundle(),
		ShouldRecord: func() bool { return true },
	})

	_, err := cman.NewClientStream(ctx, "rpc-1")
	assert.NoError(t, err)
	_, err = cman.NewClientStream(ctx, "rpc-2")
	assert.NoError(t, err)
	assert.Equal(t, c.started.Load(), int64(2))
	assert.Equal(t, c.terminated.Load(), int64(0))

	assert.NoError(t, cman.Close())
	assert.Equal(t, c.started.Load(), int64(2))
	assert.Equal(t, c.terminated.Load(), int64(2))
}

// TestManagerRejectedStreamNotCounted verifies that only a stream successfully
// added to activeStreams contributes to the lifecycle metrics.
func TestManagerRejectedStreamNotCounted(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	var c streamMetricCounters
	cman := NewWithOptions(cconn, Client, Options{
		Metrics:      c.bundle(),
		ShouldRecord: func() bool { return true },
	})
	assert.NoError(t, cman.Close())

	_, err := cman.NewClientStream(ctx, "rpc")
	assert.Error(t, err)
	assert.Equal(t, c.started.Load(), int64(0))
	assert.Equal(t, c.terminated.Load(), int64(0))
}

func TestManagerStreamLifecycleGatedOff(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()
	drainConn(ctx, sconn)

	var c streamMetricCounters
	cman := NewWithOptions(cconn, Client, Options{
		Metrics:      c.bundle(),
		ShouldRecord: func() bool { return false },
	})
	stream, err := cman.NewClientStream(ctx, "rpc")
	assert.NoError(t, err)
	assert.NoError(t, stream.Close())
	assert.NoError(t, cman.Close())

	assert.Equal(t, c.started.Load(), int64(0))
	assert.Equal(t, c.terminated.Load(), int64(0))
}
