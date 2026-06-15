// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
)

func TestQUICConfigDefaults(t *testing.T) {
	c := Options{}.quicConfig()
	require.NotNil(t, c)
	require.Equal(t, 30*time.Second, c.MaxIdleTimeout)
	require.Equal(t, 15*time.Second, c.KeepAlivePeriod) // idle/2
	require.Equal(t, int64(1<<16), c.MaxIncomingStreams)
}

func TestQUICConfigRespectsCaller(t *testing.T) {
	custom := &quic.Config{MaxIncomingStreams: 5}
	out := Options{QUIC: custom}.quicConfig()
	require.Same(t, custom, out) // caller config used as-is
	require.Equal(t, int64(5), out.MaxIncomingStreams)
}
