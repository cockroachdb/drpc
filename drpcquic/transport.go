// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"storj.io/drpc"
)

// Transport wraps a QUIC connection as a drpc.MultiplexedTransport. Each opened
// or accepted QUIC stream becomes an independent drpc.Transport.
type Transport struct {
	conn *quic.Conn
}

var _ drpc.MultiplexedTransport = (*Transport)(nil)

func newTransport(conn *quic.Conn) *Transport { return &Transport{conn: conn} }

// OpenStream opens a new outbound QUIC stream and returns it as a drpc.Transport.
func (t *Transport) OpenStream(ctx context.Context) (drpc.Transport, error) {
	s, err := t.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, mapQUICError(err)
	}
	return newStreamTransport(s), nil
}

// AcceptStream blocks until the peer opens a QUIC stream and returns it as a
// drpc.Transport.
func (t *Transport) AcceptStream(ctx context.Context) (drpc.Transport, error) {
	s, err := t.conn.AcceptStream(ctx)
	if err != nil {
		return nil, mapQUICError(err)
	}
	return newStreamTransport(s), nil
}

// Close closes the whole QUIC connection (and therefore every stream on it).
func (t *Transport) Close() error { return t.conn.CloseWithError(appNoError, "") }

// Closed returns a channel closed when the QUIC connection closes.
func (t *Transport) Closed() <-chan struct{} { return t.conn.Context().Done() }

// streamTransport adapts one *quic.Stream to drpc.Transport. It closes two gaps:
//
//   - quic-go Stream.Close() is a half-close (send-side FIN) and does NOT wake a
//     blocked Read. drpc's readers do deadline-less reads, so Close() must ALSO
//     CancelRead to release a blocked reader. Idempotent.
//   - quic-go error types are translated to drpc's recognized classes via
//     mapQUICError.
//
// Ownership invariant: whoever drives this transport (a per-RPC drpc manager) is
// the sole closer of the underlying quic.Stream.
type streamTransport struct {
	s         *quic.Stream
	closeOnce sync.Once
	closeErr  error
}

var _ drpc.Transport = (*streamTransport)(nil)

func newStreamTransport(s *quic.Stream) *streamTransport { return &streamTransport{s: s} }

func (t *streamTransport) Read(p []byte) (int, error) {
	n, err := t.s.Read(p)
	return n, mapQUICError(err)
}

func (t *streamTransport) Write(p []byte) (int, error) {
	n, err := t.s.Write(p)
	return n, mapQUICError(err)
}

// Close performs a full bidirectional teardown of the QUIC stream: it FINs the
// send side (flushing buffered writes) and CancelReads the receive side so any
// goroutine blocked in Read returns immediately. Idempotent.
func (t *streamTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.s.Close()  // half-close: send-direction FIN
		t.s.CancelRead(canceled)  // STOP_SENDING: wakes a blocked Read
	})
	return t.closeErr
}

// SetReadDeadline bounds how long a Read blocks. Used by the server to cap how
// long it waits for a client's invoke (InactivityTimeout).
func (t *streamTransport) SetReadDeadline(tm time.Time) error { return t.s.SetReadDeadline(tm) }
