// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"time"

	"github.com/quic-go/quic-go"
	"storj.io/drpc/drpcmanager"
)

// ALPN is the default QUIC application protocol negotiated for drpc-over-QUIC.
const ALPN = "drpc-quic"

const (
	// canceled is the QUIC stream error code used when drpc tears a stream down
	// (CancelRead/CancelWrite). It signals a clean, drpc-level teardown.
	canceled quic.StreamErrorCode = 0x1

	// goingAway is the QUIC application error code a server uses for a graceful
	// shutdown. Clients should treat it as retryable.
	goingAway quic.ApplicationErrorCode = 0x1

	// appNoError is the QUIC application error code for a normal connection
	// close with no error (e.g. a client closing its Conn).
	appNoError quic.ApplicationErrorCode = 0x0
)

// Options configures drpcquic clients and servers. The same value may be passed
// to Dial, Listen, and Serve; each reads the fields relevant to it (Manager for
// per-stream Reader/Writer/Stream tuning; QUIC for the underlying config; Log
// for server-side error logging).
type Options struct {
	// Manager tunes the per-stream Reader/Writer/Stream options.
	Manager drpcmanager.Options

	// QUIC is the underlying quic-go config. If nil, drpcquic defaults are
	// applied (see quicConfig). If non-nil, it is used as-is.
	QUIC *quic.Config

	// Log, if non-nil, is called by Serve for non-clean per-connection or
	// per-handler errors. Clean connection-lifecycle closes are suppressed.
	Log func(error)
}

// quicConfig returns the effective quic-go config: the caller's QUIC config if
// set, otherwise a config with drpcquic defaults (keepalive on, raised stream
// limit). Receive-window auto-tuning is left at quic-go defaults.
func (o Options) quicConfig() *quic.Config {
	if o.QUIC != nil {
		return o.QUIC
	}
	const idle = 30 * time.Second
	return &quic.Config{
		MaxIdleTimeout: idle,
		// QUIC idle timeout is connection-scoped: without keepalive, one quiet
		// period would drop every multiplexed stream on the connection.
		KeepAlivePeriod: idle / 2,
		// The default of 100 would serialize concurrent-stream workloads (and
		// the HOL benchmark) on OpenStreamSync.
		MaxIncomingStreams: 1 << 16,
	}
}
