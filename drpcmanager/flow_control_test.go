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

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// fcManagerOptions returns manager Options whose streams have flow control
// enabled via the internal stream option.
func fcManagerOptions(window, highWater, threshold int64) Options {
	stream := drpcstream.Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&stream.Internal, drpcopts.FlowControl{
		Enabled:        true,
		StreamWindow:   window,
		HighWater:      highWater,
		GrantThreshold: threshold,
	})
	return Options{Stream: stream}
}

// Enabling flow control through the manager's Stream options installs windows
// on every stream it creates (client and server), so a message larger than the
// send window only completes if credit grants flow across the real connection.
func TestManager_FlowControlEndToEnd(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	// 128 KiB window = 2 frames of initial credit.
	opts := fcManagerOptions(128<<10, 1<<20, 64<<10)

	cman := NewWithOptions(cconn, Client, opts)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, opts)
	defer func() { _ = sman.Close() }()

	// 256 KiB is four frames: two fit in the initial window, the rest can only
	// be sent as the server dispatches frames and returns credit.
	msg := make([]byte, 256<<10)

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
		assert.NoError(t, err)
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, msg))
		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		got, err := stream.RawRecv()
		assert.NoError(t, err)
		assert.Equal(t, len(got), len(msg))

		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	ctx.Wait()
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
