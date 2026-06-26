// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"storj.io/drpc"
)

func TestMapQUICError(t *testing.T) {
	require.NoError(t, mapQUICError(nil))

	// io.EOF passes through unchanged (drpc handles it as a clean end).
	require.ErrorIs(t, mapQUICError(io.EOF), io.EOF)

	// context deadline => the BARE sentinel (not wrapped), so drpc.ToRPCErr's
	// value-match maps it to codes.DeadlineExceeded rather than the net.Error
	// timeout catch-all turning it into a ClosedError/Unavailable.
	require.Equal(t, context.DeadlineExceeded, mapQUICError(context.DeadlineExceeded))
	require.Equal(t, context.DeadlineExceeded,
		mapQUICError(fmt.Errorf("read: %w", context.DeadlineExceeded))) // wrapped is unwrapped
	require.Equal(t, codes.DeadlineExceeded,
		status.Code(drpc.ToRPCErr(mapQUICError(context.DeadlineExceeded)))) // end-to-end

	// our cancel code => clean teardown => io.EOF.
	require.ErrorIs(t, mapQUICError(&quic.StreamError{ErrorCode: canceled}), io.EOF)

	// other stream error => drpc.ClosedError.
	require.True(t, drpc.ClosedError.Has(mapQUICError(&quic.StreamError{ErrorCode: 0x9})))

	// application error (peer/connection close) => drpc.ClosedError.
	require.True(t, drpc.ClosedError.Has(mapQUICError(&quic.ApplicationError{ErrorCode: goingAway})))

	// idle timeout => drpc.ClosedError.
	require.True(t, drpc.ClosedError.Has(mapQUICError(&quic.IdleTimeoutError{})))

	// net.ErrClosed => drpc.ClosedError.
	require.True(t, drpc.ClosedError.Has(mapQUICError(net.ErrClosed)))

	// unknown error passes through unchanged.
	unk := errors.New("boom")
	require.Equal(t, unk, mapQUICError(unk))
}
