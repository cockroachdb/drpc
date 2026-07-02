// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcserver

import (
	"context"

	"github.com/quic-go/quic-go"
	"github.com/zeebo/errs"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcquic"
)

// ServeQuic listens for QUIC connections on lis and serves drpc on each one.
// Each accepted connection is handled by ServeQuicConn. It returns nil on a
// clean shutdown (ctx canceled or listener closed), mirroring Serve.
func (s *Server) ServeQuic(ctx context.Context, lis *quic.Listener) (err error) {
	tracker := drpcctx.NewTracker(ctx)
	defer tracker.Wait()
	defer tracker.Cancel()

	tracker.Run(func(ctx context.Context) {
		<-ctx.Done()
		_ = lis.Close()
	})

	for {
		conn, err := lis.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errs.Wrap(err)
		}

		tracker.Run(func(ctx context.Context) {
			if err := s.ServeQuicConn(ctx, conn); err != nil && s.opts.Log != nil {
				s.opts.Log(err)
			}
		})
	}
}

// ServeQuicConn serves drpc requests on a single QUIC connection, dispatching
// each accepted QUIC stream to the server's handler. Each drpc stream maps to
// its own QUIC stream, so streams are served concurrently (one goroutine per
// stream). The per-stream message-size cap and gRPC-metadata compatibility are
// taken from the server's manager options, matching the TCP path.
func (s *Server) ServeQuicConn(ctx context.Context, conn *quic.Conn) (err error) {
	tracker := drpcctx.NewTracker(ctx)
	defer tracker.Wait()
	defer tracker.Cancel()

	// On shutdown, close the connection. This natively cancels every in-flight
	// stream's context (and thus each handler's Context()), so we don't need a
	// per-stream cancellation watcher.
	tracker.Run(func(ctx context.Context) {
		<-ctx.Done()
		_ = conn.CloseWithError(0, "")
	})

	// Capture the peer's TLS certificates from the QUIC connection so handlers
	// can authenticate the remote, mirroring ServeOne's behavior for *tls.Conn.
	peerInfo := drpcctx.PeerConnectionInfo{
		Certificates: conn.ConnectionState().TLS.PeerCertificates,
	}
	maxMsg := s.opts.Manager.Reader.MaximumBufferSize
	grpcCompat := s.opts.Manager.GRPCMetadataCompatMode

	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errs.Wrap(err)
		}

		tracker.Run(func(ctx context.Context) {
			if err := drpcquic.ServeStream(stream, s.handler, peerInfo, maxMsg, grpcCompat); err != nil {
				if s.opts.Log != nil {
					s.opts.Log(err)
				}
			}
		})
	}
}
