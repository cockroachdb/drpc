// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"errors"
	"io"
	"net"

	"github.com/quic-go/quic-go"
	"storj.io/drpc"
)

// mapQUICError translates quic-go error types into the error classes drpc's
// stack recognizes. drpc only special-cases io.EOF and *net.OpError, so without
// this translation quic-go errors would surface to applications as opaque
// wrapped values, defeating retry logic and clean cancellation.
func mapQUICError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}

	var se *quic.StreamError
	if errors.As(err, &se) {
		if se.ErrorCode == canceled {
			// our reserved teardown code: treat as a clean end of stream.
			return io.EOF
		}
		return drpc.ClosedError.Wrap(err)
	}

	var ae *quic.ApplicationError
	if errors.As(err, &ae) {
		return drpc.ClosedError.Wrap(err)
	}

	var ie *quic.IdleTimeoutError
	if errors.As(err, &ie) {
		return drpc.ClosedError.Wrap(err)
	}

	if errors.Is(err, net.ErrClosed) {
		return drpc.ClosedError.Wrap(err)
	}

	// catch-all for remaining timeout-style transport errors.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return drpc.ClosedError.Wrap(err)
	}

	return err
}
