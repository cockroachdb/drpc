// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"crypto/tls"

	"github.com/quic-go/quic-go"
	"github.com/zeebo/errs"
	"storj.io/drpc"
	"storj.io/drpc/drpcmetadata"
)

// Options configures a QuicConn.
type Options struct {
	// MaxMessageSize bounds a single received message (each message rides one
	// QUIC-stream frame). Zero uses the 4 MiB default; raise it for large
	// messages (e.g. CockroachDB's AddSSTable).
	MaxMessageSize int
}

// QuicConn adapts a QUIC connection to the drpc.Conn interface. Each Invoke or
// NewStream opens its own QUIC stream, so unlike the base drpc.Conn contract
// multiple calls may be in flight concurrently.
type QuicConn struct {
	qconn *quic.Conn
	opts  Options
}

var _ drpc.Conn = (*QuicConn)(nil)

// NewConn returns a QuicConn backed by the provided QUIC connection, using
// default options.
func NewConn(qconn *quic.Conn) *QuicConn { return &QuicConn{qconn: qconn} }

// NewConnWithOptions returns a QuicConn backed by qconn with the given options.
func NewConnWithOptions(qconn *quic.Conn, opts Options) *QuicConn {
	return &QuicConn{qconn: qconn, opts: opts}
}

// Dial opens a QUIC connection to addr and returns a QuicConn with default
// options. The DRPC ALPN is forced automatically.
func Dial(ctx context.Context, addr string, tlsConf *tls.Config) (*QuicConn, error) {
	return DialWithOptions(ctx, addr, tlsConf, Options{})
}

// DialWithOptions is Dial with explicit options (e.g. a raised MaxMessageSize).
func DialWithOptions(
	ctx context.Context, addr string, tlsConf *tls.Config, opts Options,
) (*QuicConn, error) {
	// 1-RTT quic.DialAddr, not the 0-RTT "early" variant (quic.DialAddrEarly):
	// 0-RTT early data is replayable and would require classifying RPCs as
	// replay-safe before sending on it; revisit if/when we want 0-RTT.
	qconn, err := quic.DialAddr(ctx, addr, ensureALPN(tlsConf), nil)
	if err != nil {
		return nil, err
	}
	return NewConnWithOptions(qconn, opts), nil
}

// QUICConn returns the underlying QUIC connection.
func (c *QuicConn) QUICConn() *quic.Conn { return c.qconn }

// Closed returns a channel that is closed once the connection is closed.
func (c *QuicConn) Closed() <-chan struct{} { return c.qconn.Context().Done() }

// Close closes the connection.
func (c *QuicConn) Close() error {
	return c.qconn.CloseWithError(quicConnCloseCode, "")
}

// openStream opens a QUIC stream and writes the invoke header for rpc.
func (c *QuicConn) openStream(ctx context.Context, rpc string) (*QuicStream, error) {
	metadata, err := encodeMetadata(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := c.qconn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	s := newClientStream(ctx, stream, c.opts.MaxMessageSize)
	if err := s.writeInvoke(rpc, metadata); err != nil {
		return nil, errs.Combine(err, s.Close())
	}
	s.watchCaller(ctx)
	return s, nil
}

// Invoke issues a unary rpc: it sends in, half-closes, and waits for out.
func (c *QuicConn) Invoke(
	ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message,
) (err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	s, err := c.openStream(ctx, rpc)
	if err != nil {
		return err
	}
	defer func() { err = errs.Combine(err, s.Close()) }()

	if err := s.MsgSend(in, enc); err != nil {
		return err
	}
	if err := s.CloseSend(); err != nil {
		return err
	}
	return s.MsgRecv(out, enc)
}

// NewStream begins a streaming rpc on the connection.
func (c *QuicConn) NewStream(
	ctx context.Context, rpc string, enc drpc.Encoding,
) (_ drpc.Stream, err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	s, err := c.openStream(ctx, rpc)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// encodeMetadata encodes drpc metadata from the outgoing context, if any.
func encodeMetadata(ctx context.Context) ([]byte, error) {
	md, ok := drpcmetadata.GetFromOutgoingContext(ctx)
	if !ok || len(md) == 0 {
		return nil, nil
	}
	return drpcmetadata.Encode(nil, md)
}
