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

// waitWithin fails the test if the tracker's goroutines do not all finish within
// d. drpctest.NewTracker's context has no deadline and Tracker.Wait is unbounded,
// so a regression that deadlocks the transfer would otherwise hang until the
// global go test timeout instead of failing locally. (close(done) still runs if
// Wait calls runtime.Goexit on an already-failed test, so a real failure is not
// masked by a spurious timeout.)
func waitWithin(t *testing.T, tr *drpctest.Tracker, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Wait()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for transfer to complete (possible flow-control deadlock)")
	}
}

// Enabling flow control through the manager's Stream options installs windows
// on every stream it creates (client and server). More data is sent than the
// window holds, so the transfer only completes if consume-driven credit grants
// flow back across the real connection (credit returns only after a complete
// message is read). The messages are kept window-sized here so credit cycles
// purely through consumption; the overdraft path for larger-than-window messages
// is exercised by TestManager_FlowControlOverdraftUnblocksLargeMessage.
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

	waitWithin(t, ctx, 10*time.Second)
}

// The reviewer's deadlock scenario, end to end over a real connection: a 32 KiB
// message is consumed but its credit stays withheld below the 64 KiB grant
// threshold, then a 128 KiB message equal to the whole window must still
// complete, followed by a third message. Per-frame gating would strand the
// 128 KiB message mid-way (the receiver cannot grant credit for an incomplete
// message); the message-boundary gate plus overdraft lets it finish.
func TestManager_FlowControlOverdraftUnblocksLargeMessage(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	// 128 KiB window, 64 KiB grant threshold on both ends.
	opts := fcManagerOptions(128<<10, 64<<10)
	cman := NewWithOptions(cconn, Client, opts)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, opts)
	defer func() { _ = sman.Close() }()

	sizes := []int{32 << 10, 128 << 10, 64 << 10}

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
		assert.NoError(t, err)
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		for _, n := range sizes {
			assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, make([]byte, n)))
		}
		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()
		for _, want := range sizes {
			got, err := stream.RawRecv()
			assert.NoError(t, err)
			assert.Equal(t, len(got), want)
		}
		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	waitWithin(t, ctx, 10*time.Second)
}

// Cancelling an RPC's context wakes a sender parked on flow-control credit. The
// client has flow control with a small window; the server does not, so it never
// returns credit. A first message spends the whole window, so the next message
// parks at its boundary (gating is per message under overdraft, not per frame).
// This exercises the production wake path: manageStream sees the cancellation
// and calls Cancel -> terminate -> sigs.send, the send window's done signal.
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

	// Accept the stream and drain whatever arrives until teardown.
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

	// Spend the whole window on a first message (the server returns no credit).
	assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, make([]byte, 128<<10)))

	// The next message parks on its first frame: the window is exhausted and no
	// grant is coming.
	done := make(chan error, 1)
	go func() { done <- stream.RawWrite(drpcwire.KindMessage, make([]byte, 64<<10)) }()

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
//
// The client->server message is larger than the default grant threshold, so if
// flow control were accidentally enabled with default sizes the server's receive
// side would emit a grant while consuming it (a smaller message never reaches the
// threshold and would leave the tripwire toothless). The server's small response
// is a same-direction acknowledged marker on the server->client stream: the
// client's sniffer signals when it has *classified* that ack. Waiting for that
// signal -- rather than for RawRecv, which returns once the bytes are copied into
// the parser's buffer, before the classifying switch runs -- guarantees any
// earlier server->client window update was classified too, since the parser
// processes frames in order. Asserting before any manager is closed also avoids
// the MuxWriter dropping a still-buffered frame on Stop.
func TestManager_DefaultConfigEmitsNoWindowUpdates(t *testing.T) {
	tr := drpctest.NewTracker(t)
	defer tr.Close()

	var sawMessage, sawWindowUpdate atomic.Bool
	// The server->client sniffer signals here once it classifies a KindMessage
	// (the ack); buffered + non-blocking so the parser never stalls on it.
	ackClassified := make(chan struct{}, 1)
	markAck := func() {
		select {
		case ackClassified <- struct{}{}:
		default:
		}
	}
	sniff := func(c net.Conn, onMessage func()) net.Conn {
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
					if onMessage != nil {
						onMessage()
					}
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

	cman := NewWithOptions(sniff(cconn, markAck), Client, Options{})
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sniff(sconn, nil), Server, Options{})
	defer func() { _ = sman.Close() }()

	tr.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()
		got, err := stream.RawRecv()
		assert.NoError(t, err)
		assert.Equal(t, len(got), 1<<20)
		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
		// Respond after consuming the message: this is the marker the client
		// waits on. Any accidental grant would have been written before it.
		assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, []byte("ack")))
	})

	stream, err := cman.NewClientStream(context.Background(), "rpc", drpc.CompressionNone)
	assert.NoError(t, err)
	assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
	// 1 MiB exceeds the 512 KiB default GrantThreshold, so an accidentally
	// enabled default-config receive side would emit a grant here.
	assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, make([]byte, 1<<20)))
	assert.NoError(t, stream.CloseSend())

	got, err := stream.RawRecv()
	assert.NoError(t, err)
	assert.Equal(t, string(got), "ack")

	// RawRecv returning only means the ack bytes were copied into the parser's
	// buffer, not that its classifying switch ran. Wait for the server->client
	// sniffer to signal it classified the ack; by in-order parsing, any earlier
	// window update is classified by then.
	select {
	case <-ackClassified:
	case <-time.After(10 * time.Second):
		t.Fatal("sniffer did not classify the acknowledgement")
	}

	assert.That(t, sawMessage.Load())
	assert.That(t, !sawWindowUpdate.Load())

	waitWithin(t, tr, 10*time.Second)
}
