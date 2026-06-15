// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
	"storj.io/drpc"
)

func TestMapQUICError(t *testing.T) {
	require.NoError(t, mapQUICError(nil))

	// io.EOF passes through unchanged (drpc handles it as a clean end).
	require.ErrorIs(t, mapQUICError(io.EOF), io.EOF)

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
