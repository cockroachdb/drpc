// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"net"

	"github.com/quic-go/quic-go"
)

// Listener accepts QUIC connections and yields each as a drpc
// MultiplexedTransport.
type Listener struct {
	lis *quic.Listener
}

// Accept blocks for the next QUIC connection and returns it as a *Transport.
// The error is returned unwrapped so callers can detect quic.ErrServerClosed.
func (l *Listener) Accept(ctx context.Context) (*Transport, error) {
	conn, err := l.lis.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return newTransport(conn), nil
}

// Addr returns the local address the listener is bound to.
func (l *Listener) Addr() net.Addr { return l.lis.Addr() }

// Close stops accepting new connections. Already-accepted connections are
// unaffected.
func (l *Listener) Close() error { return l.lis.Close() }
