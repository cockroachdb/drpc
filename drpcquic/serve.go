// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"errors"

	"github.com/quic-go/quic-go"

	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcserver"
)

// Server serves drpc over QUIC. Construct it with the same *drpcserver.Server
// you would use for TCP (via drpcserver.New / NewWithOptions), then call Serve
// with a QUIC Listener obtained from Listen / ListenPacket.
//
// Server mirrors the shape of (*drpcserver.Server).Serve(ctx, net.Listener):
// Serve takes a context and a listener. The listener type differs by necessity —
// QUIC runs over UDP and accepts connections that each multiplex many streams,
// which a net.Listener (a stream of byte-pipe net.Conn) cannot represent.
type Server struct {
	srv  *drpcserver.Server
	opts Options
}

// NewServer returns a QUIC Server that serves the given drpc server, using opts
// for the listener (Accept) lifecycle and error logging.
func NewServer(srv *drpcserver.Server, opts Options) *Server {
	return &Server{srv: srv, opts: opts}
}

// Serve accepts QUIC connections on lis and serves drpc on each (one
// ServeMultiplexed per connection, each handling its streams concurrently). It
// returns when ctx is canceled or the listener is closed, after in-flight
// connections and handlers drain. opts.Log, if set, receives non-clean errors.
func (s *Server) Serve(ctx context.Context, lis *Listener) error {
	tracker := drpcctx.NewTracker(ctx)
	defer tracker.Wait()
	defer tracker.Cancel()

	for {
		tr, err := lis.Accept(ctx)
		if err != nil {
			if errors.Is(err, quic.ErrServerClosed) || ctx.Err() != nil {
				return err
			}
			if s.opts.Log != nil {
				s.opts.Log(err)
			}
			continue
		}
		tracker.Run(func(ctx context.Context) {
			if serr := s.srv.ServeMultiplexed(ctx, tr); serr != nil && s.opts.Log != nil {
				s.opts.Log(serr)
			}
		})
	}
}

// Serve is a convenience wrapper around NewServer(srv, opts).Serve(ctx, lis).
func Serve(ctx context.Context, lis *Listener, srv *drpcserver.Server, opts Options) error {
	return NewServer(srv, opts).Serve(ctx, lis)
}
