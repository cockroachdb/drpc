// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// fcManagerOptions returns manager Options whose streams have flow control
// enabled via the internal stream option.
func fcManagerOptions(window, threshold int64) Options {
	stream := drpcstream.Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&stream.Internal, drpcopts.FlowControl{
		Enabled:        true,
		StreamWindow:   window,
		GrantThreshold: threshold,
	})
	return Options{Stream: stream}
}

// Enabling flow control through the manager's Stream options installs windows
// on every stream it creates (client and server). More data is sent than the
// window holds, so the transfer only completes if consume-driven credit grants
// flow back across the real connection. Under grant-on-consume a single
// message must fit in the window (credit returns only after a complete message
// is read), so the volume is spread over several window-sized messages.
func TestManager_FlowControlEndToEnd(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	// 128 KiB window = two 64 KiB messages of initial credit.
	opts := fcManagerOptions(128<<10, 64<<10)

	cman := NewWithOptions(cconn, Client, opts)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, opts)
	defer func() { _ = sman.Close() }()

	// Four 64 KiB messages: two spend the initial window; the rest can only be
	// sent as the server consumes messages and returns credit.
	const msgs, msgLen = 4, 64 << 10

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
		assert.NoError(t, err)
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		for i := 0; i < msgs; i++ {
			assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, make([]byte, msgLen)))
		}
		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		for i := 0; i < msgs; i++ {
			got, err := stream.RawRecv()
			assert.NoError(t, err)
			assert.Equal(t, len(got), msgLen)
		}
		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	ctx.Wait()
}

// Cancelling an RPC's context wakes a sender parked on flow-control credit. The
// client has flow control with a small window; the server does not, so it never
// returns credit and the client's oversized send parks. This exercises the
// production wake path: manageStream sees the cancellation and calls
// Cancel -> terminate -> sigs.send, the send window's done signal.
func TestManager_FlowControlContextCancelWakesParkedSend(t *testing.T) {
	tr := drpctest.NewTracker(t)
	defer tr.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := NewWithOptions(cconn, Client, fcManagerOptions(128<<10, 64<<10))
	defer func() { _ = cman.Close() }()
	// Server has no flow control, so it never returns credit.
	sman := New(sconn, Server)
	defer func() { _ = sman.Close() }()

	// Accept the stream and drain; the message never completes (the sender
	// parks mid-message), so RawRecv just blocks until teardown.
	tr.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		if err != nil {
			return
		}
		defer func() { _ = stream.Close() }()
		for {
			if _, err := stream.RawRecv(); err != nil {
				return
			}
		}
	})

	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := cman.NewClientStream(streamCtx, "rpc", drpc.CompressionNone)
	assert.NoError(t, err)
	assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))

	// A 512 KiB message exceeds the 128 KiB window; once the initial credit is
	// spent the send parks waiting for grants that never come.
	done := make(chan error, 1)
	go func() { done <- stream.RawWrite(drpcwire.KindMessage, make([]byte, 512<<10)) }()

	select {
	case <-done:
		t.Fatal("send returned before it could park on credit")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("parked send did not wake on context cancellation")
	}
}

// sniffConn tees every byte read off the connection into w, where a frame
// parser classifies it.
type sniffConn struct {
	net.Conn
	w io.Writer
}

func (s sniffConn) Read(p []byte) (int, error) {
	n, err := s.Conn.Read(p)
	if n > 0 {
		_, _ = s.w.Write(p[:n])
	}
	return n, err
}

// Tripwire for accidental enablement: a default-configuration connection must
// never put a KindWindowUpdate on the wire. Both directions are sniffed; the
// KindMessage assertion proves the sniffer actually parsed the traffic.
func TestManager_DefaultConfigEmitsNoWindowUpdates(t *testing.T) {
	tr := drpctest.NewTracker(t)
	defer tr.Close()

	var sawMessage, sawWindowUpdate atomic.Bool
	sniff := func(c net.Conn) net.Conn {
		pr, pw := io.Pipe()
		go func() {
			rd := drpcwire.NewReader(pr)
			for {
				fr, err := rd.ReadFrame()
				if err != nil {
					_, _ = io.Copy(io.Discard, pr) // keep the tee unblocked
					return
				}
				switch fr.Kind {
				case drpcwire.KindMessage:
					sawMessage.Store(true)
				case drpcwire.KindWindowUpdate:
					sawWindowUpdate.Store(true)
				}
			}
		}()
		t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
		return sniffConn{Conn: c, w: pw}
	}

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := NewWithOptions(sniff(cconn), Client, Options{})
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sniff(sconn), Server, Options{})
	defer func() { _ = sman.Close() }()

	tr.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()
		got, err := stream.RawRecv()
		assert.NoError(t, err)
		assert.Equal(t, len(got), 256<<10)
		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	stream, err := cman.NewClientStream(context.Background(), "rpc", drpc.CompressionNone)
	assert.NoError(t, err)
	assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
	assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, make([]byte, 256<<10)))
	assert.NoError(t, stream.Close())
	tr.Wait()

	assert.That(t, sawMessage.Load())
	assert.That(t, !sawWindowUpdate.Load())
}
