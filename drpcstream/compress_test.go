package drpcstream

import (
	"context"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// TestHandlePacket_Decompress verifies that a Snappy-compressed message frame
// is transparently decompressed before being returned by RawRecv.
func TestHandlePacket_Decompress(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	mw := testMuxWriter(t)
	st := NewWithOptions(ctx, 1, mw, NewBufferPool(), Options{Compression: drpc.CompressionSnappy})

	original := []byte("hello compression")
	compressed := drpcwire.Compress(drpc.CompressionSnappy, nil, original)

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Kind: drpcwire.KindMessage,
		Data: compressed,
		Done: true,
	}))

	got := <-recv
	assert.DeepEqual(t, got, original)
}

// TestHandlePacket_NoCompression confirms that a stream without compression
// delivers raw message payloads through RawRecv unchanged.
func TestHandlePacket_NoCompression(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	mw := testMuxWriter(t)
	st := New(ctx, 1, mw, NewBufferPool())

	payload := []byte("raw payload")

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Kind: drpcwire.KindMessage,
		Data: payload,
		Done: true,
	}))

	got := <-recv
	assert.DeepEqual(t, got, payload)
}

// TestRawRecv_DecompressionError verifies that receiving invalid compressed
// data returns a ProtocolError rather than silently delivering garbage.
func TestRawRecv_DecompressionError(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	mw := testMuxWriter(t)
	st := NewWithOptions(ctx, 1, mw, NewBufferPool(), Options{Compression: drpc.CompressionSnappy})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Kind: drpcwire.KindMessage,
		Data: []byte("not valid snappy data"),
		Done: true,
	}))

	_, err := st.RawRecv()
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
}

// TestRawRecv_DecompressedDataIsCopied ensures each decompressed message gets
// its own copy, so the internal decompression buffer can be safely reused
// without corrupting previously received data.
func TestRawRecv_DecompressedDataIsCopied(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	mw := testMuxWriter(t)
	st := NewWithOptions(ctx, 1, mw, NewBufferPool(), Options{Compression: drpc.CompressionSnappy})

	msg1 := []byte("message one")
	msg2 := []byte("message two")
	compressed1 := drpcwire.Compress(drpc.CompressionSnappy, nil, msg1)
	compressed2 := drpcwire.Compress(drpc.CompressionSnappy, nil, msg2)

	recv := make(chan []byte, 2)
	ctx.Run(func(ctx context.Context) {
		for i := 0; i < 2; i++ {
			data, err := st.RawRecv()
			assert.NoError(t, err)
			recv <- data
		}
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: compressed1, Done: true,
	}))
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 2}, Kind: drpcwire.KindMessage, Data: compressed2, Done: true,
	}))

	got1 := <-recv
	got2 := <-recv
	assert.DeepEqual(t, got1, msg1)
	assert.DeepEqual(t, got2, msg2)
}

// TestRawWrite_NoCompression verifies that RawWrite succeeds on a stream
// with no compression configured.
func TestRawWrite_NoCompression(t *testing.T) {
	mw := testMuxWriter(t)
	st := New(context.Background(), 1, mw, nil)
	err := st.RawWrite(drpcwire.KindMessage, []byte("hello"))
	assert.NoError(t, err)
}

// TestRawWrite_WithCompression verifies that RawWrite succeeds when Snappy
// compression is enabled on the stream.
func TestRawWrite_WithCompression(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, nil, Options{Compression: drpc.CompressionSnappy})
	err := st.RawWrite(drpcwire.KindMessage, []byte("hello"))
	assert.NoError(t, err)
}

// chanWriter captures each Write call on a channel without blocking.
type chanWriter struct{ wrote chan []byte }

func (w *chanWriter) Write(p []byte) (int, error) {
	w.wrote <- append([]byte(nil), p...)
	return len(p), nil
}

// TestRawRecv_DecompressionError_SendErrorReachesWire verifies that after a
// decompression failure the stream remains open long enough for SendError to
// transmit a KindError frame to the peer, rather than silently terminating.
func TestRawRecv_DecompressionError_SendErrorReachesWire(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cw := &chanWriter{wrote: make(chan []byte, 16)}
	mw := drpcwire.NewMuxWriter(cw, func(error) {})
	t.Cleanup(func() { mw.Stop(nil); <-mw.Done() })

	st := NewWithOptions(ctx, 1, mw, NewBufferPool(), Options{Compression: drpc.CompressionSnappy})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Kind: drpcwire.KindMessage,
		Data: []byte("not valid snappy data"),
		Done: true,
	}))

	_, recvErr := st.RawRecv()
	assert.Error(t, recvErr)
	assert.That(t, drpc.ProtocolError.Has(recvErr))

	// The stream must not be terminated yet — otherwise SendError is a no-op
	// and no error frame reaches the client.
	assert.That(t, !st.IsTerminated())

	// Simulate what handleRPC does: send the error back to the client.
	sendErr := st.SendError(recvErr)
	assert.NoError(t, sendErr)

	// Verify the KindError frame actually reached the wire.
	waitForKind(t, cw.wrote, drpcwire.KindError)
}
