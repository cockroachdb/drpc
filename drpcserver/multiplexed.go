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
	tracker := drpcctx.NewTracker(ctx)

	// Shutdown order matters (defers run LIFO, so this runs Cancel → Close →
	// Wait). A per-stream goroutine parked in ServerStream's deadline-less invoke
	// read is NOT released by ctx cancellation — only by closing the connection.
	// So Close() must run BEFORE Wait(); otherwise a stream stalled mid-invoke at
	// shutdown would hang Wait() forever. Cancel() first lets in-flight handlers
	// unwind via their stream watchers before the connection is torn down.
	defer tracker.Wait()
	defer func() { err = errs.Combine(err, man.Close()) }()
	defer tracker.Cancel()

	for {
		// Accept only: this returns as soon as a peer opens a stream and must NOT
		// read the invoke here. Reading the invoke in the accept loop would let one
		// stream that is slow to send its invoke head-of-line every later stream on
		// this connection — reintroducing the blocking QUIC is meant to remove.
		tr, serr := man.AcceptTransport(ctx)
		if serr != nil {
			if isCleanClose(serr) {
				return nil
			}
			return errs.Wrap(serr)
		}
		// Read the invoke and serve the RPC in the stream's own goroutine, so that
		// one stream's invoke read never blocks accepting or serving another.
		tracker.Run(func(ctx context.Context) {
			stream, rpc, serr := man.ServerStream(ctx, tr)
			if serr != nil {
				// A failed or slow stream affects only itself; the connection keeps
				// serving other streams. ServerStream has already closed tr.
				if !isCleanClose(serr) && s.opts.Log != nil {
					s.opts.Log(serr)
				}
				return
			}
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
