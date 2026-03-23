// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"

	"storj.io/drpc"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// handleFrame is a helper that sends a single-frame packet to the stream.
// It constructs a frame with the given kind, matching the stream's ID,
// using the provided message ID, done=true.
func handleFrame(st *Stream, kind drpcwire.Kind, mid uint64) error {
	return st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: st.ID(), Message: mid},
		Kind: kind,
		Done: true,
	})
}

func TestStream_StateTransitions(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	any := errors.New("any sentinel error")

	checkErrs := func(t *testing.T, exp interface{}, got error) {
		t.Helper()

		if cl, ok := exp.(*errs.Class); ok {
			assert.That(t, cl.Has(got))
		} else {
			switch exp {
			case any:
				assert.Error(t, got)
			case nil:
				assert.NoError(t, got)
			default:
				assert.Equal(t, exp, got)
			}
		}
	}

	cases := []struct {
		Op   func(st *Stream) error
		Send interface{}
		Recv error
	}{
		{ // send close
			Op:   func(st *Stream) error { return st.Close() },
			Send: any,
			Recv: any,
		},

		{ // send error
			Op:   func(st *Stream) error { return st.SendError(errors.New("test")) },
			Send: io.EOF,
			Recv: any,
		},

		{ // send closesend
			Op:   func(st *Stream) error { return st.CloseSend() },
			Send: any,
			Recv: nil,
		},

		{ // recv cancel
			Op:   func(st *Stream) error { st.Cancel(context.Canceled); return nil },
			Send: io.EOF,
			Recv: context.Canceled,
		},

		{ // recv deadline
			Op:   func(st *Stream) error { st.Cancel(context.DeadlineExceeded); return nil },
			Send: io.EOF,
			Recv: context.DeadlineExceeded,
		},

		{ // recv close
			Op:   func(st *Stream) error { return handleFrame(st, drpcwire.KindClose, 1) },
			Send: &drpc.ClosedError,
			Recv: io.EOF,
		},

		{ // recv error
			Op:   func(st *Stream) error { return handleFrame(st, drpcwire.KindError, 1) },
			Send: io.EOF,
			Recv: any,
		},

		{ // recv closesend
			Op:   func(st *Stream) error { return handleFrame(st, drpcwire.KindCloseSend, 1) },
			Send: nil,
			Recv: io.EOF,
		},
	}

	for _, test := range cases {
		st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))
		assert.NoError(t, test.Op(st))

		checkErrs(t, test.Send, st.RawWrite(drpcwire.KindMessage, nil))

		if test.Recv == nil {
			ctx.Run(func(ctx context.Context) { _ = handleFrame(st, drpcwire.KindMessage, 2) })
		}
		_, err := st.RawRecv()
		checkErrs(t, test.Recv, err)
	}
}

func TestStream_Unblocks(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cases := []struct {
		Op func(st *Stream) error
	}{
		{ // send close
			Op: func(st *Stream) error { return st.Close() },
		},

		{ // send error
			Op: func(st *Stream) error { return st.SendError(errors.New("test")) },
		},

		{ // recv cancel
			Op: func(st *Stream) error { st.Cancel(context.Canceled); return nil },
		},

		{ // recv deadline
			Op: func(st *Stream) error { st.Cancel(context.DeadlineExceeded); return nil },
		},

		{ // recv close
			Op: func(st *Stream) error { return handleFrame(st, drpcwire.KindClose, 1) },
		},

		{ // recv error
			Op: func(st *Stream) error { return handleFrame(st, drpcwire.KindError, 1) },
		},

		{ // recv closesend
			Op: func(st *Stream) error { return handleFrame(st, drpcwire.KindCloseSend, 1) },
		},
	}

	for _, test := range cases {
		st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

		ctx.Run(func(ctx context.Context) { _, _ = st.RawRecv() })
		assert.NoError(t, test.Op(st))
		ctx.Wait()
	}
}

func TestStream_ContextCancel(t *testing.T) {
	ctx := context.Background()
	st := New(ctx, 0, drpcwire.NewWriter(io.Discard, 0))

	child, cancel := context.WithCancel(st.Context())
	defer cancel()

	assert.NoError(t, st.Close())
	<-st.Context().Done()
	<-child.Done()
}

func TestStream_ConcurrentCloseCancel(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	defer func() { _ = pw.Close() }()

	st := New(ctx, 0, drpcwire.NewWriter(pw, 0))

	// start the Close call
	errch := make(chan error, 1)
	go func() { errch <- st.Close() }()

	// wait for the close to begin writing
	_, err := pr.Read(make([]byte, 1))
	assert.NoError(t, err)

	// cancel the context and close the transport
	st.Cancel(context.Canceled)
	assert.NoError(t, pw.Close())

	// we should always receive the canceled error
	assert.That(t, errors.Is(<-errch, context.Canceled))
}

func TestStream_CorkUntilFirstRead(t *testing.T) {
	run := func() {
		ctx := drpctest.NewTracker(t)
		defer ctx.Close()

		var buf bytes.Buffer
		st := New(ctx, 0, drpcwire.NewWriter(&buf, 50))

		// concurrently read and write at the same time.
		// we should always see the write happen.

		errch := make(chan error, 3)
		ctx.Run(func(ctx context.Context) {
			errch <- st.MsgSend([]byte("write"), byteEncoding{})
		})
		ctx.Run(func(ctx context.Context) {
			_, err := st.RawRecv()
			errch <- err
		})
		ctx.Run(func(ctx context.Context) {
			errch <- st.HandleFrame(drpcwire.Frame{
				Data: []byte("read"),
				ID:   drpcwire.ID{Message: 1},
				Kind: drpcwire.KindMessage,
				Done: true,
			})
		})

		assert.NoError(t, <-errch)
		assert.NoError(t, <-errch)
		assert.NoError(t, <-errch)

		assert.Equal(t, buf.String(), "\x05\x00\x01\x05write")
	}
	for i := 0; i < 100; i++ {
		run()
	}
}

type byteEncoding struct{}

func (byteEncoding) Marshal(msg drpc.Message) ([]byte, error) { return msg.([]byte), nil }
func (byteEncoding) Unmarshal(buf []byte, msg drpc.Message) error {
	*msg.(*[]byte) = append(*msg.(*[]byte), buf...)
	return nil
}

func TestStream_PacketBufferReuse(t *testing.T) {
	run := func() {
		ctx := drpctest.NewTracker(t)
		defer ctx.Close()
		defer ctx.Wait()

		data := make([]byte, 20)
		mid := uint64(1)
		st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

		ctx.Run(func(ctx context.Context) {
			for !st.IsTerminated() {
				err := st.HandleFrame(drpcwire.Frame{
					Data: data,
					ID:   drpcwire.ID{Stream: 1, Message: mid},
					Kind: drpcwire.KindMessage,
					Done: true,
				})
				if err != nil {
					return
				}
				mid++
				for i := range data {
					data[i]++
				}
			}
		})

		ctx.Run(func(ctx context.Context) {
			for !st.IsTerminated() {
				_, err := st.RawRecv()
				if err != nil {
					return
				}
			}
		})

		ctx.Run(func(ctx context.Context) {
			st.Cancel(context.Canceled)
		})
	}

	for i := 0; i < 100; i++ {
		run()
	}
}

func TestStream_SendCancelBusyDuringBlockedClose(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	defer func() { _ = pw.Close() }()

	st := New(ctx, 0, drpcwire.NewWriter(pw, 0))

	// launch a goroutine to close the stream
	ctx.Run(func(ctx context.Context) { _ = st.Close() })

	// read just 1 byte from the pipe to ensure that the Close has started
	_, err := pr.Read(make([]byte, 1))
	assert.NoError(t, err)
	assert.That(t, st.IsTerminated())

	// even though the stream is terminated, soft cancel should report that
	// the stream is still busy because the close is being sent.
	busy, err := st.SendCancel(context.Canceled)
	assert.NoError(t, err)
	assert.That(t, busy)
}

//
// HandleFrame tests
//

// A frame routed to the wrong stream is a protocol error.
func TestHandleFrame_WrongStreamID(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))

	err := st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: 2, Message: 1},
		Kind: drpcwire.KindMessage,
		Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "doesn't belong"))
}

// A frame with a message ID lower than a previously completed message is rejected.
func TestHandleFrame_MessageMonotonicity(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))
	// Close packet buffer so KindMessage delivery doesn't block.
	st.pbuf.Close(io.EOF)

	// m3 completes, nextMessageID becomes 4.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 3}, Kind: drpcwire.KindMessage, Done: true,
	}))

	// m2 < 4 → error.
	err := st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 2}, Kind: drpcwire.KindMessage, Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "monotonicity"))
}

func TestHandleFrame_FirstFrameOnFreshStream(t *testing.T) {
	// On the client side, the first message received will have ID 1. But on the
	// server side, invoke is consumed by the manager. The first frame reaching
	// the stream could have msg > 1 (e.g., msg=2). nextMessageID=1, so 2 > 1
	// makes this a valid frame.
	for _, messageID := range []uint64{1, 2} {
		st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))
		// Close the packet buffer so KindMessage Put doesn't block.
		st.pbuf.Close(io.EOF)
		err := st.HandleFrame(drpcwire.Frame{
			ID: drpcwire.ID{Stream: 1, Message: messageID}, Kind: drpcwire.KindMessage, Done: true,
		})
		assert.NoError(t, err)
	}
}

// When a higher message ID arrives mid-assembly, the in-progress data is
// silently discarded and a new packet begins.
func TestHandleFrame_HigherMsgDiscardsInProgress(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

	// Start accumulating m1 (done=false doesn't call Put, so no blocking).
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: []byte("discard"), Done: false,
	}))

	// Launch receiver before sending done frame to avoid Put blocking.
	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	// m2 arrives, m1 data should be silently discarded.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 2}, Kind: drpcwire.KindMessage, Data: []byte("kept"), Done: true,
	}))

	// Verify only m2's data was delivered.
	assert.DeepEqual(t, <-recv, []byte("kept"))
}

// Continuation frames (same message ID, mid-assembly) must carry the same
// kind as the first frame. A kind change mid-packet is a protocol error.
func TestHandleFrame_KindChangeWithinPacket(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Done: false,
	}))

	err := st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindError, Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "kind change"))
}

// Multiple continuation frames for the same message accumulate data correctly.
func TestHandleFrame_MultiFrameDataAccumulation(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

	// Continuation frames (done=false) don't call Put, so no blocking.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: []byte("hel"), Done: false,
	}))
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: []byte("lo "), Done: false,
	}))

	// Launch receiver before the final frame to avoid Put blocking.
	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: []byte("world"), Done: true,
	}))

	assert.DeepEqual(t, <-recv, []byte("hello world"))
}

// Multi-frame assembly works when the message ID is greater than nextMessageID
// (e.g., on the server side where invoke consumed earlier message IDs).
// Continuation frames must accumulate data, not reset on each frame.
func TestHandleFrame_MultiFrameWithSkippedMessageID(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

	// msg=3 is greater than nextMessageID=1. Continuation frames for the
	// same message must still accumulate correctly.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 3}, Kind: drpcwire.KindMessage, Data: []byte("hel"), Done: false,
	}))
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 3}, Kind: drpcwire.KindMessage, Data: []byte("lo"), Done: false,
	}))

	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 3}, Kind: drpcwire.KindMessage, Data: []byte(" world"), Done: true,
	}))

	assert.DeepEqual(t, <-recv, []byte("hello world"))
}

// Once a message completes (done=true), the same message ID is rejected.
func TestHandleFrame_DonePreventsReplay(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))
	// Close packet buffer so KindMessage delivery doesn't block.
	st.pbuf.Close(io.EOF)

	// m1 completes → nextMessageID becomes 2.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Done: true,
	}))

	// Same message ID again → error.
	err := st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "monotonicity"))
}

// Kind consistency is only enforced within a packet (continuation frames), not
// across messages. A multi-frame KindMessage followed by a KindClose for the
// next message should be accepted without error.
func TestHandleFrame_MultiFrameThenNextMessage(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: []byte("ab"), Done: false,
	}))

	// Launch receiver before done frame.
	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Data: []byte("cd"), Done: true,
	}))
	assert.DeepEqual(t, <-recv, []byte("abcd"))

	// Message 2 with a different kind — should not trigger kind check.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 2}, Kind: drpcwire.KindClose, Done: true,
	}))

	// Close triggers EOF on recv.
	ctx.Run(func(ctx context.Context) {
		_, err := st.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})
	ctx.Wait()
}

// Invoke and InvokeMetadata frames are rejected on an already-created stream.
func TestHandleFrame_InvokeOnExistingStream(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))

	err := handleFrame(st, drpcwire.KindInvoke, 1)
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "invoke on existing stream"))
}

func TestHandleFrame_InvokeMetadataOnExistingStream(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))

	err := handleFrame(st, drpcwire.KindInvokeMetadata, 1)
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "invoke on existing stream"))
}

// Frames arriving after the stream is terminated are silently ignored.
func TestHandleFrame_AfterTerminated(t *testing.T) {
	st := New(context.Background(), 1, drpcwire.NewWriter(io.Discard, 0))

	// Terminate the stream via cancel.
	st.Cancel(context.Canceled)

	// Frames after termination are silently ignored.
	err := st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindMessage, Done: true,
	})
	assert.NoError(t, err)
}

// A completed KindMessage frame delivers its data through RawRecv.
func TestHandleFrame_MessageDeliveredViaRecv(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	st := New(ctx, 1, drpcwire.NewWriter(io.Discard, 0))

	// Launch receiver before sending to avoid Put blocking.
	recv := make(chan []byte, 1)
	ctx.Run(func(ctx context.Context) {
		data, err := st.RawRecv()
		assert.NoError(t, err)
		recv <- data
	})

	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Kind: drpcwire.KindMessage,
		Data: []byte("payload"),
		Done: true,
	}))

	assert.DeepEqual(t, <-recv, []byte("payload"))
}

//
// Write-side tests
//

func TestRawWrite_NonMessageSingleFrame(t *testing.T) {
	// Non-KindMessage kinds must produce a single frame (n=0 in
	// rawWriteLocked means default 64KB, effectively no split for
	// small payloads). Verify they produce exactly one frame with Done=true.
	kinds := []drpcwire.Kind{
		drpcwire.KindInvoke,
		drpcwire.KindError,
		drpcwire.KindCancel,
		drpcwire.KindClose,
		drpcwire.KindCloseSend,
		drpcwire.KindInvokeMetadata,
	}

	for _, kind := range kinds {
		var buf bytes.Buffer
		st := New(context.Background(), 1, drpcwire.NewWriter(&buf, 0))

		assert.NoError(t, st.RawWrite(kind, []byte("data")))
		assert.NoError(t, st.RawFlush())
		var err error

		// Parse all frames from the buffer — should be exactly one.
		data := buf.Bytes()
		var frames []drpcwire.Frame
		for len(data) > 0 {
			var fr drpcwire.Frame
			var ok bool
			data, fr, ok, err = drpcwire.ParseFrame(data)
			assert.NoError(t, err)
			assert.That(t, ok)
			frames = append(frames, fr)
		}
		assert.Equal(t, len(frames), 1)
		assert.That(t, frames[0].Done)
		assert.Equal(t, frames[0].Kind, kind)
	}
}

func TestRawWrite_MessageRespectsSplitSize(t *testing.T) {
	var buf bytes.Buffer
	st := NewWithOptions(context.Background(), 1,
		drpcwire.NewWriter(&buf, 0),
		Options{SplitSize: 5},
	)

	// "helloworld" is 10 bytes, split at 5 → 2 frames.
	assert.NoError(t, st.RawWrite(drpcwire.KindMessage, []byte("helloworld")))
	assert.NoError(t, st.RawFlush())
	var err error

	data := buf.Bytes()
	var frames []drpcwire.Frame
	for len(data) > 0 {
		var fr drpcwire.Frame
		var ok bool
		data, fr, ok, err = drpcwire.ParseFrame(data)
		assert.NoError(t, err)
		assert.That(t, ok)
		frames = append(frames, fr)
	}
	assert.Equal(t, len(frames), 2)
	assert.That(t, !frames[0].Done)
	assert.That(t, frames[1].Done)
	assert.DeepEqual(t, frames[0].Data, []byte("hello"))
	assert.DeepEqual(t, frames[1].Data, []byte("world"))
}
