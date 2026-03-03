// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
	grpcmetadata "google.golang.org/grpc/metadata"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

func TestTimeout(t *testing.T) {
	tr := make(blockingTransport)
	man := NewWithOptions(tr, Options{
		InactivityTimeout: time.Millisecond,
	})
	defer func() { _ = man.Close() }()

	_, _, err := man.NewServerStream(context.Background())
	assert.That(t, errors.Is(err, context.DeadlineExceeded))
}

func TestDrpcMetadata(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn)
	defer func() { _ = cman.Close() }()

	sman := NewWithOptions(sconn, Options{
		GRPCMetadataCompatMode: false,
	})
	defer func() { _ = sman.Close() }()

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc")
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		md := map[string]string{"key": "value", "multi-value-key": "value1,value2"}
		var buf []byte
		buf, err = drpcmetadata.Encode(buf, md)
		assert.NoError(t, err)
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvokeMetadata, buf))
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("invoke")))
		assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, []byte("message")))
		assert.NoError(t, stream.RawFlush())

		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		streamCtx := stream.Context()

		drpcMd, ok := drpcmetadata.GetFromIncomingContext(streamCtx)
		assert.That(t, ok)
		assert.Equal(t, drpcMd, map[string]string{"key": "value", "multi-value-key": "value1,value2"})

		grpcMd, ok := grpcmetadata.FromIncomingContext(streamCtx)
		assert.False(t, ok)
		assert.Nil(t, grpcMd)

		defer func() { _ = stream.Close() }()

		_, err = stream.RawRecv()
		assert.NoError(t, err)

		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	ctx.Wait()
}

func TestDrpcMetadataWithGRPCMetadataCompatMode(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn)
	defer func() { _ = cman.Close() }()

	sman := NewWithOptions(sconn, Options{
		GRPCMetadataCompatMode: true,
	})
	defer func() { _ = sman.Close() }()

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc")
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		md := map[string]string{"key": "value", "multi-value-key": "value1,value2"}
		var buf []byte
		buf, err = drpcmetadata.Encode(buf, md)
		assert.NoError(t, err)
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvokeMetadata, buf))
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("invoke")))
		assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, []byte("message")))
		assert.NoError(t, stream.RawFlush())

		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		streamCtx := stream.Context()

		drpcMd, ok := drpcmetadata.GetFromIncomingContext(streamCtx)
		assert.False(t, ok)
		assert.Nil(t, drpcMd)

		grpcMd, ok := grpcmetadata.FromIncomingContext(streamCtx)
		assert.That(t, ok)
		assert.Equal(t, grpcMd, grpcmetadata.MD{"key": []string{"value"},
			"multi-value-key": []string{"value1,value2"}})

		defer func() { _ = stream.Close() }()

		_, err = stream.RawRecv()
		assert.NoError(t, err)

		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	ctx.Wait()
}

func TestDrpcMetadataInterleavedAcrossStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn)
	defer func() { _ = cman.Close() }()

	sman := NewWithOptions(sconn, Options{
		GRPCMetadataCompatMode: false,
	})
	defer func() { _ = sman.Close() }()

	stream1, err := cman.NewClientStream(ctx, "rpc-1")
	assert.NoError(t, err)
	defer func() { _ = stream1.Close() }()

	stream2, err := cman.NewClientStream(ctx, "rpc-2")
	assert.NoError(t, err)
	defer func() { _ = stream2.Close() }()

	metadata1 := map[string]string{"stream": "one"}
	metadata2 := map[string]string{"stream": "two"}

	buf1, err := drpcmetadata.Encode(nil, metadata1)
	assert.NoError(t, err)
	buf2, err := drpcmetadata.Encode(nil, metadata2)
	assert.NoError(t, err)

	assert.NoError(t, stream1.RawWrite(drpcwire.KindInvokeMetadata, buf1))
	assert.NoError(t, stream2.RawWrite(drpcwire.KindInvokeMetadata, buf2))
	assert.NoError(t, stream1.RawWrite(drpcwire.KindInvoke, []byte("rpc-1")))
	assert.NoError(t, stream2.RawWrite(drpcwire.KindInvoke, []byte("rpc-2")))

	srvStream1, rpc1, err := sman.NewServerStream(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "rpc-1", rpc1)
	defer func() { _ = srvStream1.Close() }()

	got1, ok := drpcmetadata.GetFromIncomingContext(srvStream1.Context())
	assert.That(t, ok)
	assert.Equal(t, metadata1, got1)

	srvStream2, rpc2, err := sman.NewServerStream(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "rpc-2", rpc2)
	defer func() { _ = srvStream2.Close() }()

	got2, ok := drpcmetadata.GetFromIncomingContext(srvStream2.Context())
	assert.That(t, ok)
	assert.Equal(t, metadata2, got2)
}

func TestNewServerStreamUnreadMessageDoesNotBlockOtherStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn)
	defer func() { _ = cman.Close() }()

	sman := New(sconn)
	defer func() { _ = sman.Close() }()

	stream1, err := cman.NewClientStream(ctx, "rpc-1")
	assert.NoError(t, err)
	defer func() { _ = stream1.Close() }()

	stream2, err := cman.NewClientStream(ctx, "rpc-2")
	assert.NoError(t, err)
	defer func() { _ = stream2.Close() }()

	assert.NoError(t, stream1.RawWrite(drpcwire.KindInvoke, []byte("rpc-1")))
	assert.NoError(t, stream1.RawWrite(drpcwire.KindMessage, []byte("message-1")))
	assert.NoError(t, stream2.RawWrite(drpcwire.KindInvoke, []byte("rpc-2")))

	srvStream1, rpc1, err := sman.NewServerStream(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "rpc-1", rpc1)
	defer func() { _ = srvStream1.Close() }()

	// Do not read the first stream's message. The manager must still be able to
	// accept and register additional streams.
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	srvStream2, rpc2, err := sman.NewServerStream(timeoutCtx)
	assert.NoError(t, err)
	assert.Equal(t, "rpc-2", rpc2)
	defer func() { _ = srvStream2.Close() }()
}

// TestConcurrentLargeMessages verifies that two streams writing messages larger
// than SplitSize concurrently do not corrupt each other's data. With the current
// implementation, rawWriteLocked splits messages into multiple frames and each
// frame is appended to the shared write buffer independently. Frames from
// different streams can interleave in the buffer, and the reader resets partial
// packets when it sees a frame from a different stream, silently corrupting data.
func TestConcurrentLargeMessages(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	// Use a small SplitSize to force even small messages to be split into
	// multiple frames, making interleaving likely.
	streamOpts := drpcstream.Options{SplitSize: 5}

	cman := NewWithOptions(cconn, Options{Stream: streamOpts})
	defer func() { _ = cman.Close() }()

	sman := NewWithOptions(sconn, Options{Stream: streamOpts})
	defer func() { _ = sman.Close() }()

	// Create two client streams and send invoke + message concurrently.
	stream1, err := cman.NewClientStream(ctx, "rpc-1")
	assert.NoError(t, err)
	defer func() { _ = stream1.Close() }()

	stream2, err := cman.NewClientStream(ctx, "rpc-2")
	assert.NoError(t, err)
	defer func() { _ = stream2.Close() }()

	msg1 := []byte("AAAAAAAAAAAAAAAAAAAA") // 20 bytes, split into 4 frames of 5 bytes
	msg2 := []byte("BBBBBBBBBBBBBBBBBBBB") // 20 bytes, split into 4 frames of 5 bytes

	// Send invokes first (these are small, no splitting).
	assert.NoError(t, stream1.RawWrite(drpcwire.KindInvoke, []byte("rpc-1")))
	assert.NoError(t, stream2.RawWrite(drpcwire.KindInvoke, []byte("rpc-2")))

	// Accept both server streams before sending messages, so the streams are
	// registered and the reader can route packets.
	srvStream1, rpc1, err := sman.NewServerStream(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "rpc-1", rpc1)
	defer func() { _ = srvStream1.Close() }()

	srvStream2, rpc2, err := sman.NewServerStream(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "rpc-2", rpc2)
	defer func() { _ = srvStream2.Close() }()

	// Write messages concurrently from both streams. With SplitSize=5, each
	// 20-byte message becomes 4 frames. The frames should not interleave.
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-ready
		assert.NoError(t, stream1.RawWrite(drpcwire.KindMessage, msg1))
	}()
	go func() {
		defer wg.Done()
		<-ready
		assert.NoError(t, stream2.RawWrite(drpcwire.KindMessage, msg2))
	}()
	close(ready)
	wg.Wait()

	// Read from both server streams and verify correctness.
	got1, err := srvStream1.RawRecv()
	assert.NoError(t, err)
	assert.DeepEqual(t, got1, msg1)

	got2, err := srvStream2.RawRecv()
	assert.NoError(t, err)
	assert.DeepEqual(t, got2, msg2)
}

type blockingTransport chan struct{}

func (b blockingTransport) Read(p []byte) (n int, err error)  { <-b; return 0, io.EOF }
func (b blockingTransport) Write(p []byte) (n int, err error) { <-b; return 0, io.EOF }
func (b blockingTransport) Close() error                      { close(b); return nil }

func TestUnblocked_NoCancel(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn)
	defer func() { _ = cman.Close() }()

	sman := New(sconn)
	defer func() { _ = sman.Close() }()

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc")
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("invoke")))
		assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, []byte("message")))
		assert.NoError(t, stream.RawFlush())

		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		_, err = stream.RawRecv()
		assert.NoError(t, err)

		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	ctx.Wait()
}

func TestUnblocked_SoftCancel(t *testing.T) {
	run := func(t *testing.T, softCancel bool) {
		ctx := drpctest.NewTracker(t)
		defer ctx.Close()

		tr := newBlockedTransport()
		man := NewWithOptions(tr, Options{SoftCancel: softCancel})
		defer func() { _ = man.Close() }()
		defer tr.setReadOpen(true)
		defer tr.setWriteOpen(true)

		for i := 0; i < 10; i++ {
			func() {
				subctx, cancel := context.WithCancel(ctx)
				defer cancel()

				stream, err := man.NewClientStream(subctx, "rpc")
				if softCancel {
					assert.NoError(t, err)
				} else if i > 0 {
					// Hard cancel terminates the connection, so subsequent streams fail.
					assert.Error(t, err)
					return
				} else {
					assert.NoError(t, err)
				}
				defer func() { _ = stream.Close() }()

				cancel()

				// temporary unblock writing to allow the stream to finish soft cancel
				tr.setWriteOpen(true)
				// With multiplexing, we wait for the stream to finish instead of Unblocked().
				<-stream.Finished()
				tr.setWriteOpen(false)
			}()
		}
	}

	t.Run("Enabled", func(t *testing.T) { run(t, true) })
	t.Run("Disabled", func(t *testing.T) { run(t, false) })
}

type blockedTransport struct {
	mu *sync.Mutex
	co *sync.Cond
	ro bool
	wo bool
}

func newBlockedTransport() *blockedTransport {
	mu := new(sync.Mutex)
	co := sync.NewCond(mu)
	return &blockedTransport{
		mu: mu,
		co: co,
	}
}

func (b *blockedTransport) setWriteOpen(open bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.wo = open
	b.co.Broadcast()
}

func (b *blockedTransport) setReadOpen(open bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.ro = open
	b.co.Broadcast()
}

func (b *blockedTransport) wait(p int, rw *bool) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for !*rw {
		b.co.Wait()
	}
	return p, nil
}

func (b *blockedTransport) Read(p []byte) (n int, err error)  { return b.wait(len(p), &b.ro) }
func (b *blockedTransport) Write(p []byte) (n int, err error) { return b.wait(len(p), &b.wo) }
func (b *blockedTransport) Close() error                      { return nil }
