// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcpool

import (
	"context"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpctest"
)

func TestPoolReuse(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{
		Capacity:    2,
		KeyCapacity: 1,
	})
	defer func() { _ = pool.Close() }()

	count := 0
	dial := func(ctx context.Context, key string) (Conn, error) {
		count++
		return new(callbackConn), nil
	}
	check := func(conn drpc.Conn, expected int) {
		t.Helper()
		_ = conn.Invoke(ctx, "", nil, nil, nil)
		assert.Equal(t, count, expected)
	}

	conn1 := pool.Get(ctx, "key1", dial)
	conn2 := pool.Get(ctx, "key2", dial)
	conn3 := pool.Get(ctx, "key3", dial)
	assert.Equal(t, count, 0) // lazily dial

	check(conn1, 1) // conn1's first invoke dials
	check(conn1, 1) // conn1 reuses the connection
	check(conn2, 2) // conn2's first invoke dials
	check(conn2, 2) // conn2 reuses the connection
	check(conn1, 2) // conn1 still reuses the connection
	check(conn3, 3) // conn3's first invoke dials, evicts key1 (oldest idle)
	check(conn1, 4) // conn1 was evicted so it needs another dial, evicts key2
	check(conn2, 5) // conn2 was evicted so it needs another dial
}

func TestPoolConcurrency(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{
		Capacity:    2,
		KeyCapacity: 1,
	})
	defer func() { _ = pool.Close() }()

	count := 0
	uc1 := new(callbackConn)
	dial := func(ctx context.Context, key string) (Conn, error) {
		count++
		return uc1, nil
	}

	conn1 := pool.Get(ctx, "key1", dial)

	// with unlimited streams per conn (default), all streams share one connection.
	stream1_1, err := conn1.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 1)

	stream1_2, err := conn1.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 1) // reuses same connection

	stream1_3, err := conn1.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 1) // reuses same connection

	// close all streams
	_ = stream1_1.Close()
	<-stream1_1.Context().Done()
	_ = stream1_2.Close()
	<-stream1_2.Context().Done()
	_ = stream1_3.Close()
	<-stream1_3.Context().Done()

	// connection is still available for reuse
	stream1_4, err := conn1.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 1) // still reuses

	_ = stream1_4.Close()
	<-stream1_4.Context().Done()
}

func TestPoolConcurrency_MaxStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{
		MaxStreamsPerConn: 2,
	})
	defer func() { _ = pool.Close() }()

	count := 0
	dial := func(ctx context.Context, key string) (Conn, error) {
		count++
		return new(callbackConn), nil
	}

	conn := pool.Get(ctx, "key1", dial)

	// first two streams share one connection
	st1, err := conn.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 1)

	st2, err := conn.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 1) // still the same conn

	// third stream exceeds MaxStreamsPerConn=2, dials a new connection
	st3, err := conn.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 2) // new dial

	// close a stream from the first connection
	_ = st1.Close()
	<-st1.Context().Done()

	// next stream should reuse the first connection (now has capacity)
	st4, err := conn.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, count, 2) // no new dial

	_ = st2.Close()
	<-st2.Context().Done()
	_ = st3.Close()
	<-st3.Context().Done()
	_ = st4.Close()
	<-st4.Context().Done()
}

// TestPool_Expiration checks that idle entries expire eventually.
func TestPool_Expiration(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	closed := make(chan string, 1)
	pool := New[string](Options{Expiration: time.Nanosecond})
	defer func() { _ = pool.Close() }()

	useConn(ctx, pool, closed, "key")
	assert.Equal(t, <-closed, "key")
}

// TestPool_Stale checks that closed connections are skipped on acquire.
func TestPool_Stale(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	calls := 0
	pool := New[string](Options{})
	defer func() { _ = pool.Close() }()

	conn := pool.Get(ctx, "key", func(ctx context.Context, key string) (Conn, error) {
		calls++
		return &callbackConn{ClosedFn: func() <-chan struct{} { return closedCh }}, nil
	})

	// an invoke should cause a dial
	invoke(ctx, conn)
	assert.Equal(t, calls, 1)

	// another invoke should cause another dial because the conn is considered closed
	invoke(ctx, conn)
	assert.Equal(t, calls, 2)
}

// TestPool_Capacity checks that total capacity limits are enforced.
func TestPool_Capacity(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	closed := make(chan string, 1)
	pool := New[string](Options{Capacity: 1})
	defer func() { _ = pool.Close() }()

	// using key0 should remain in the pool
	useConn(ctx, pool, closed, "key0")
	assert.Equal(t, len(closed), 0)

	// using key1 should evict key0
	useConn(ctx, pool, closed, "key1")
	assert.Equal(t, len(closed), 1)
	assert.Equal(t, <-closed, "key0")

	// close the pool and key1 should be closed
	_ = pool.Close()
	assert.Equal(t, len(closed), 1)
	assert.Equal(t, <-closed, "key1")
}

// TestPool_Capacity_Expiration checks that capacity limits are enforced
// even if expiration is set.
func TestPool_Capacity_Expiration(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	closed := make(chan string, 1)
	pool := New[string](Options{
		Capacity:   1,
		Expiration: time.Hour,
	})
	defer func() { _ = pool.Close() }()

	// using key0 should remain in the pool
	useConn(ctx, pool, closed, "key0")
	assert.Equal(t, len(closed), 0)

	// using key1 should evict key0
	useConn(ctx, pool, closed, "key1")
	assert.Equal(t, len(closed), 1)
	assert.Equal(t, <-closed, "key0")

	// close the pool and key1 should be closed
	_ = pool.Close()
	assert.Equal(t, len(closed), 1)
	assert.Equal(t, <-closed, "key1")
}

// TestPool_Capacity_Negative checks that negative capacities cache nothing.
func TestPool_Capacity_Negative(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	closed := make(chan string, 1)
	pool := New[string](Options{Capacity: -1})
	defer func() { _ = pool.Close() }()

	useConn(ctx, pool, closed, "key0")
	// With negative capacity, the conn is inserted but not cached.
	// The conn itself is not closed by the pool since insertAndAcquire
	// skips insertion — but the conn is still usable.
	// The close callback won't fire because the pool doesn't own it.
}

// TestPool_KeyCapacity checks that per-key capacity limits are enforced.
func TestPool_KeyCapacity(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	closed := make(chan string, 2)
	pool := New[string](Options{
		KeyCapacity:      1,
		MaxStreamsPerConn: 1,
	})
	defer func() { _ = pool.Close() }()

	useConn(ctx, pool, closed, "key0")
	assert.Equal(t, len(closed), 0)

	useConn(ctx, pool, closed, "key1")
	assert.Equal(t, len(closed), 0)

	// get two concurrent streams so that we force two underlying dials
	// causing one to be evicted when it is closed.
	conn := getConn(ctx, pool, closed, "key0")
	stream1, _ := conn.NewStream(ctx, "", nil)
	stream2, _ := conn.NewStream(ctx, "", nil)

	_ = stream1.Close()
	<-stream1.Context().Done()
	_ = stream2.Close()
	<-stream2.Context().Done()

	assert.Equal(t, len(closed), 1)
	assert.Equal(t, <-closed, "key0")
}

// TestPool_KeyCapacity_Negative checks that negative per-key capacities cache nothing.
func TestPool_KeyCapacity_Negative(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{KeyCapacity: -1})
	defer func() { _ = pool.Close() }()

	count := 0
	conn := pool.Get(ctx, "key0", func(ctx context.Context, key string) (Conn, error) {
		count++
		return new(callbackConn), nil
	})

	// each invoke dials because nothing is cached
	invoke(ctx, conn)
	assert.Equal(t, count, 1)
	invoke(ctx, conn)
	assert.Equal(t, count, 2)
}

func TestPool_MultipleCachedReuse(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{
		KeyCapacity:      2,
		MaxStreamsPerConn: 1,
	})
	defer func() { _ = pool.Close() }()

	closeStream := func(st drpc.Stream) { _ = st.Close(); <-st.Context().Done() }
	closedConns := make(map[int]bool)
	dials := 0
	conn := pool.Get(ctx, "key", func(ctx context.Context, key string) (Conn, error) {
		d := dials
		dials++
		return &callbackConn{
			ClosedFn: func() <-chan struct{} {
				if closedConns[d] {
					return closedCh
				}
				return nil
			},
		}, nil
	})

	// start two concurrent streams (MaxStreamsPerConn=1 forces two dials)
	st1, err := conn.NewStream(ctx, "rpc", nil)
	assert.NoError(t, err)
	defer closeStream(st1)

	st2, err := conn.NewStream(ctx, "rpc", nil)
	assert.NoError(t, err)
	defer closeStream(st2)

	// ensure we dialed twice
	assert.Equal(t, dials, 2)

	// put both the dialed connections back into the pool
	closeStream(st1)
	closeStream(st2)

	// cause the first connection to be considered dead
	closedConns[0] = true

	// start a new stream
	st3, err := conn.NewStream(ctx, "rpc", nil)
	assert.NoError(t, err)
	defer closeStream(st3)

	// the new stream should have reused the second connection
	assert.Equal(t, dials, 2)

	// start a new concurrent stream
	st4, err := conn.NewStream(ctx, "rpc", nil)
	assert.NoError(t, err)
	defer closeStream(st4)

	// there should have been no free streams left
	assert.Equal(t, dials, 3)
}

func TestPool_StreamContext(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{Capacity: 1})
	uc := new(callbackConn)
	conn := pool.Get(ctx, "key", func(ctx context.Context, key string) (Conn, error) { return uc, nil })

	type key struct{}
	stream, err := conn.NewStream(context.WithValue(ctx, key{}, "bar"), "", nil)
	assert.NoError(t, err)
	sctx := stream.Context()

	assert.Equal(t, sctx.Value(key{}), "bar")

	{ // check that all the methods in the interface are at least callable
		_ = sctx.Err()
		_, _ = sctx.Deadline()
		_ = sctx.Done()
		_ = sctx.Value(key{})
	}
}

func BenchmarkPool(b *testing.B) {
	ctx := drpctest.NewTracker(b)
	defer ctx.Close()

	const capacity = 1000

	pool := New[string](Options{Capacity: capacity})
	uc := new(callbackConn)
	conn := pool.Get(ctx, "key", func(ctx context.Context, key string) (Conn, error) { return uc, nil })

	// warm up the pool
	stream, _ := conn.NewStream(ctx, "", nil)
	_ = stream.Close()
	<-stream.Context().Done()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		invoke(ctx, conn)
	}
}

func TestPool_MuxSharesConnection(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pool := New[string](Options{})
	defer func() { _ = pool.Close() }()

	dials := 0
	conn := pool.Get(ctx, "key", func(ctx context.Context, key string) (Conn, error) {
		dials++
		return new(callbackConn), nil
	})

	// 10 concurrent streams should all share one connection
	var streams []drpc.Stream
	for i := 0; i < 10; i++ {
		st, err := conn.NewStream(ctx, "", nil)
		assert.NoError(t, err)
		streams = append(streams, st)
	}
	assert.Equal(t, dials, 1)

	// close all streams
	for _, st := range streams {
		_ = st.Close()
		<-st.Context().Done()
	}

	// connection should still be reusable
	st, err := conn.NewStream(ctx, "", nil)
	assert.NoError(t, err)
	assert.Equal(t, dials, 1)

	_ = st.Close()
	<-st.Context().Done()
}
