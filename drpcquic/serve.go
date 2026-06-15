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

// Serve accepts QUIC connections on lis and serves drpc on each (one
// ServeMultiplexed per connection, each handling its streams concurrently). It
// returns when ctx is canceled or the listener is closed, after in-flight
// connections and handlers drain. opts.Log, if set, receives non-clean errors.
func Serve(ctx context.Context, lis *Listener, srv *drpcserver.Server, opts Options) error {
	tracker := drpcctx.NewTracker(ctx)
	defer tracker.Wait()
	defer tracker.Cancel()

	for {
		tr, err := lis.Accept(ctx)
		if err != nil {
			if errors.Is(err, quic.ErrServerClosed) || ctx.Err() != nil {
				return err
			}
			if opts.Log != nil {
				opts.Log(err)
			}
			continue
		}
		tracker.Run(func(ctx context.Context) {
			if serr := srv.ServeMultiplexed(ctx, tr); serr != nil && opts.Log != nil {
				opts.Log(serr)
			}
		})
	}
}
