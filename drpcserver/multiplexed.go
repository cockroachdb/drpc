// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcserver

import (
	"context"
	"errors"
	"io"

	"github.com/zeebo/errs"

	"storj.io/drpc"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcmanager"
)

// ServeMultiplexed serves drpc over a MultiplexedTransport (e.g. a QUIC
// connection). Unlike ServeOne, RPCs are handled concurrently — each accepted
// stream is an independent RPC. It returns when ctx is canceled or the
// connection dies, after in-flight handlers drain.
func (s *Server) ServeMultiplexed(ctx context.Context, mt drpc.MultiplexedTransport) (err error) {
	man := drpcmanager.NewQUIC(mt, s.opts.Manager)
	defer func() { err = errs.Combine(err, man.Close()) }()

	tracker := drpcctx.NewTracker(ctx)
	defer tracker.Wait()
	defer tracker.Cancel()

	for {
		stream, rpc, serr := man.NewServerStream(ctx)
		if serr != nil {
			if isCleanClose(serr) {
				return nil
			}
			return errs.Wrap(serr)
		}
		tracker.Run(func(ctx context.Context) {
			if rpcErr := s.handleRPC(stream, rpc); rpcErr != nil && s.opts.Log != nil {
				s.opts.Log(rpcErr)
			}
		})
	}
}

// isCleanClose reports whether a NewServerStream error represents a normal end
// of the connection (peer closed / idle / shutdown) rather than a fault.
func isCleanClose(err error) bool {
	return errors.Is(err, io.EOF) ||
		drpc.ClosedError.Has(err) ||
		errors.Is(err, context.Canceled)
}
