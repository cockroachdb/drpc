package drpcstream

import (
	"context"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

var compressionVariants = []struct {
	name string
	c    drpc.Compression
}{
	{"snappy", drpc.CompressionSnappy},
	{"minlz-fastest", drpc.CompressionMinLZFastest},
}

// TestHandlePacket_DecompressAllVariants runs the compressed receive path for
// every supported compression variant.
func TestHandlePacket_DecompressAllVariants(t *testing.T) {
	for _, v := range compressionVariants {
		t.Run(v.name, func(t *testing.T) {
			ctx := drpctest.NewTracker(t)
			defer ctx.Close()

			mw := testMuxWriter(t)
			st := NewWithOptions(ctx, 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{Compression: v.c})

			original := []byte("hello compression across all variants")
			compressed := drpcwire.Compress(v.c, nil, original)

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

			assert.NoError(t, st.RawWrite(drpcwire.KindMessage, original))
		})
	}
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
	for _, v := range compressionVariants {
		t.Run(v.name, func(t *testing.T) {
			ctx := drpctest.NewTracker(t)
			defer ctx.Close()

			mw := testMuxWriter(t)
			st := NewWithOptions(ctx, 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{Compression: v.c})

			assert.NoError(t, st.HandleFrame(drpcwire.Frame{
				ID:   drpcwire.ID{Stream: 1, Message: 1},
				Kind: drpcwire.KindMessage,
				Data: []byte("\xff\xfe\xfd not valid compressed data"),
				Done: true,
			}))

			_, err := st.RawRecv()
			assert.Error(t, err)
			assert.That(t, drpc.ProtocolError.Has(err))
		})
	}
}

// TestRawRecv_DecompressedDataIsCopied ensures each decompressed message gets
// its own copy, so the internal decompression buffer can be safely reused
// without corrupting previously received data.
func TestRawRecv_DecompressedDataIsCopied(t *testing.T) {
	for _, v := range compressionVariants {
		t.Run(v.name, func(t *testing.T) {
			ctx := drpctest.NewTracker(t)
			defer ctx.Close()

			mw := testMuxWriter(t)
			st := NewWithOptions(ctx, 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{Compression: v.c})

			msg1 := []byte("message one")
			msg2 := []byte("message two")
			compressed1 := drpcwire.Compress(v.c, nil, msg1)
			compressed2 := drpcwire.Compress(v.c, nil, msg2)

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
		})
	}
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
	st := NewWithOptions(context.Background(), 1, mw, nil, drpcmetrics.ConnectionMetrics{}, Options{Compression: drpc.CompressionSnappy})
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
	for _, v := range compressionVariants {
		t.Run(v.name, func(t *testing.T) {
			ctx := drpctest.NewTracker(t)
			defer ctx.Close()

			cw := &chanWriter{wrote: make(chan []byte, 16)}
			mw := drpcwire.NewMuxWriter(cw, func(error) {})
			t.Cleanup(func() { mw.Stop(nil); <-mw.Done() })

			st := NewWithOptions(ctx, 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{Compression: v.c})

			assert.NoError(t, st.HandleFrame(drpcwire.Frame{
				ID:   drpcwire.ID{Stream: 1, Message: 1},
				Kind: drpcwire.KindMessage,
				Data: []byte("\xff\xfe\xfd not valid compressed data"),
				Done: true,
			}))

			_, recvErr := st.RawRecv()
			assert.Error(t, recvErr)
			assert.That(t, drpc.ProtocolError.Has(recvErr))

			assert.That(t, !st.IsTerminated())

			sendErr := st.SendError(recvErr)
			assert.NoError(t, sendErr)

			waitForKind(t, cw.wrote, drpcwire.KindError)
		})
	}
}
