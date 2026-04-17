// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
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

// testMuxWriter creates a MuxWriter that writes to io.Discard with a no-op
// error handler. The writer goroutine is stopped when the test finishes.
func testMuxWriter(t *testing.T) *drpcwire.MuxWriter {
	t.Helper()
	mw := drpcwire.NewMuxWriter(io.Discard, func(error) {})
	t.Cleanup(func() { mw.Stop(nil); <-mw.Done() })
	return mw
}

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

	mw := testMuxWriter(t)
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
		st := New(ctx, 1, mw, NewBufferPool())
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

	mw := testMuxWriter(t)

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
		st := New(ctx, 1, mw, NewBufferPool())

		ctx.Run(func(ctx context.Context) { _, _ = st.RawRecv() })
		assert.NoError(t, test.Op(st))
		ctx.Wait()
	}
}

func TestStream_ContextCancel(t *testing.T) {
	ctx := context.Background()
	mw := testMuxWriter(t)
	st := New(ctx, 0, mw, NewBufferPool())

	child, cancel := context.WithCancel(st.Context())
	defer cancel()

	assert.NoError(t, st.Close())
	<-st.Context().Done()
	<-child.Done()
}

func TestStream_ConcurrentCloseCancel(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	mw := testMuxWriter(t)
	st := New(ctx, 0, mw, NewBufferPool())

	// Close and Cancel concurrently should not panic or deadlock.
	errch := make(chan error, 1)
	go func() { errch <- st.Close() }()

	st.Cancel(context.Canceled)

	// Close returns nil or context.Canceled depending on timing.
	err := <-errch
	if err != nil {
		assert.That(t, errors.Is(err, context.Canceled))
	}
}

func TestStream_PacketBufferReuse(t *testing.T) {
	run := func() {
		ctx := drpctest.NewTracker(t)
		defer ctx.Close()
		defer ctx.Wait()

		mw := testMuxWriter(t)
		data := make([]byte, 20)
		mid := uint64(1)
		st := New(ctx, 1, mw, NewBufferPool())

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

//
// HandleFrame tests
//

func TestHandleFrame_FirstFrameOnFreshStream(t *testing.T) {
	mw := testMuxWriter(t)
	for _, messageID := range []uint64{1, 2} {
		st := New(context.Background(), 1, mw, NewBufferPool())
		// Close the ring buffer so KindMessage Enqueue doesn't block.
		st.recvQueue.Close(io.EOF)
		err := st.HandleFrame(drpcwire.Frame{
			ID: drpcwire.ID{Stream: 1, Message: messageID}, Kind: drpcwire.KindMessage, Done: true,
		})
		assert.NoError(t, err)
	}
}

// Invoke and InvokeMetadata frames are rejected on an already-created stream.
func TestHandleFrame_InvokeOnExistingStream(t *testing.T) {
	mw := testMuxWriter(t)
	st := New(context.Background(), 1, mw, NewBufferPool())

	err := handleFrame(st, drpcwire.KindInvoke, 1)
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "invoke on existing stream"))
}

func TestHandleFrame_InvokeMetadataOnExistingStream(t *testing.T) {
	mw := testMuxWriter(t)
	st := New(context.Background(), 1, mw, NewBufferPool())

	err := handleFrame(st, drpcwire.KindInvokeMetadata, 1)
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "invoke on existing stream"))
}

// Frames arriving after the stream is terminated are silently ignored.
func TestHandleFrame_AfterTerminated(t *testing.T) {
	mw := testMuxWriter(t)
	st := New(context.Background(), 1, mw, NewBufferPool())

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

	mw := testMuxWriter(t)
	st := New(ctx, 1, mw, NewBufferPool())

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
