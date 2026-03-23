// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/zeebo/assert"
	grpcmetadata "google.golang.org/grpc/metadata"
	"storj.io/drpc"
	"storj.io/drpc/drpcmetadata"

	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

func TestDrpcMetadata(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn, Client)
	defer func() { _ = cman.Close() }()

	sman := NewWithOptions(sconn, Server, Options{
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

	cman := New(cconn, Client)
	defer func() { _ = cman.Close() }()

	sman := NewWithOptions(sconn, Server, Options{
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

// writeFrames serializes the given frames and writes them to w.
func writeFrames(t *testing.T, w io.Writer, frames ...drpcwire.Frame) {
	t.Helper()
	var buf []byte
	for _, fr := range frames {
		buf = drpcwire.AppendFrame(buf, fr)
	}
	_, err := w.Write(buf)
	assert.NoError(t, err)
}

// createFrame is a shorthand for constructing a Frame.
func createFrame(kind drpcwire.Kind, sid, mid uint64, data string, done bool) drpcwire.Frame {
	return drpcwire.Frame{
		ID:   drpcwire.ID{Stream: sid, Message: mid},
		Kind: kind,
		Data: []byte(data),
		Done: done,
	}
}

// waitForClosed blocks until the manager terminates or the timeout expires.
func waitForClosed(t *testing.T, man *Manager) {
	t.Helper()
	select {
	case <-man.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not terminate in time")
	}
}

//
// manageReader tests
//

// Global frame monotonicity: a frame with an ID lower than the last seen
// frame causes the manager to terminate with a protocol error.
func TestManageReader_GlobalMonotonicity_SameStream(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	// Consume the invoke and drain messages so HandleFrame doesn't block.
	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		for {
			if _, err := stream.RawRecv(); err != nil {
				return
			}
		}
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 5, "ok", true),
		createFrame(drpcwire.KindMessage, 1, 4, "bad", true),
	)

	waitForClosed(t, man)
}

// Invoke replay: after [s1,m1,invoke,done=true], lastFrameID is bumped to
// {1,2}. A replayed [s1,m1,invoke] is caught by the monotonicity check.
func TestManageReader_InvokeReplayBlocked(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	ctx.Run(func(ctx context.Context) {
		_, _, _ = man.NewServerStream(ctx)
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
	)

	waitForClosed(t, man)
}

// Non-done frames don't bump the message ID, so continuation frames with
// the same ID are accepted.
func TestManageReader_ContinuationFramesAccepted(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		data, err := stream.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 2, "hel", false),
		createFrame(drpcwire.KindMessage, 1, 2, "lo", true),
	)

	assert.DeepEqual(t, <-recv, []byte("hello"))
}

// Old-stream frames are silently ignored when the stream has been cancelled
// and removed from the registry.
func TestManageReader_OldStreamFramesIgnored(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn, Client)
	defer func() { _ = cman.Close() }()

	// Drain all client writes so nothing blocks.
	ctx.Run(func(ctx context.Context) {
		buf := make([]byte, 4096)
		for {
			_, err := sconn.Read(buf)
			if err != nil {
				return
			}
		}
	})

	// Create stream 1 on the client, then cancel it so it's removed
	// from the registry.
	subctx, cancel := context.WithCancel(ctx)
	stream1, err := cman.NewClientStream(subctx, "rpc1")
	assert.NoError(t, err)
	cancel()
	<-stream1.Finished()

	stream2, err := cman.NewClientStream(ctx, "rpc2")
	assert.NoError(t, err)

	// Write an old-stream frame (s1) then the real response for s2.
	// The s1 frame should be silently ignored by the client manager.
	writeFrames(t, sconn,
		createFrame(drpcwire.KindMessage, 1, 1, "old", true),
		createFrame(drpcwire.KindMessage, 2, 1, "new", true),
	)

	data, err := stream2.RawRecv()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("new"))

	_ = stream2.Close()
}

// A valid invoke sequence: Invoke → Message.
// Metadata encoding is covered separately by TestDrpcMetadata.
func TestManageReader_ValidInvokeSequence(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		stream, rpc, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		assert.Equal(t, rpc, "myrpc")

		data, err := stream.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "myrpc", true),
		createFrame(drpcwire.KindMessage, 1, 2, "payload", true),
	)

	assert.DeepEqual(t, <-recv, []byte("payload"))
}

// Multi-frame message delivered through manager to stream: frames are
// assembled by the stream into a single packet.
func TestManageReader_MultiFrameDelivery(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)

		data, err := stream.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 2, "hel", false),
		createFrame(drpcwire.KindMessage, 1, 2, "lo ", false),
		createFrame(drpcwire.KindMessage, 1, 2, "world", true),
	)

	assert.DeepEqual(t, <-recv, []byte("hello world"))
}

// When a higher message ID arrives mid-assembly, the partial data is
// discarded and only the new message is delivered.
func TestManageReader_HigherMsgDiscardsInProgress(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		data, err := stream.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 2, "discard", false),
		createFrame(drpcwire.KindMessage, 1, 3, "kept", true),
	)

	assert.DeepEqual(t, <-recv, []byte("kept"))
}

// A continuation frame with a different kind than the first frame of the
// packet causes the manager to terminate with a protocol error.
func TestManageReader_KindChangeWithinPacket(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		for {
			if _, err := stream.RawRecv(); err != nil {
				return
			}
		}
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 2, "data", false),
		createFrame(drpcwire.KindClose, 1, 2, "", true),
	)

	waitForClosed(t, man)
}

// Multi-frame assembly works correctly when the message ID is greater than
// the previous message (e.g., on the server side where invoke consumed
// earlier IDs).
func TestManageReader_MultiFrameWithSkippedMessageID(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		data, err := stream.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 3, "hel", false),
		createFrame(drpcwire.KindMessage, 1, 3, "lo", true),
	)

	assert.DeepEqual(t, <-recv, []byte("hello"))
}

// A second invoke for the same stream ID is rejected — the stream treats
// it as a protocol error, terminating the manager.
func TestManageReader_InvokeOnExistingStream(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)
		_ = stream
	})

	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc1", true),
		createFrame(drpcwire.KindInvoke, 1, 2, "rpc2", true),
	)

	waitForClosed(t, man)
	assert.That(t, drpc.ProtocolError.Has(man.sigs.term.Err()))
}

// When a non-invoke frame arrives before the stream is created (e.g.,
// NewServerStream hasn't returned yet), manageReader waits for the stream
// and retries.
func TestManageReader_WaitsForStreamCreation(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	man := New(sconn, Server)
	defer func() { _ = man.Close() }()

	// Write invoke + message immediately. The message arrives before
	// NewServerStream creates the stream, exercising the default/wait path.
	writeFrames(t, cconn,
		createFrame(drpcwire.KindInvoke, 1, 1, "rpc", true),
		createFrame(drpcwire.KindMessage, 1, 2, "data", true),
	)

	// Small delay to let manageReader process both frames.
	time.Sleep(10 * time.Millisecond)

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		stream, _, err := man.NewServerStream(ctx)
		assert.NoError(t, err)

		data, err := stream.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.DeepEqual(t, <-recv, []byte("data"))
}
