// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcopts

import (
	"storj.io/drpc"
	"storj.io/drpc/drpcstats"
)

// Stream contains internal options for the drpcstream package.
type Stream struct {
	transport   drpc.Transport
	fin         chan<- struct{}
	kind        drpc.StreamKind
	rpc         string
	stats       *drpcstats.Stats
	flowControl FlowControl
}

// FlowControl configures per-stream flow control. Internal-only until the
// CockroachDB version-gated enablement is ready.
//
// Memory note: a message larger than StreamWindow is finished by overdrafting
// (see drpcstream), so the receiver may hold up to MaxMessageSize for a single
// in-flight message rather than only StreamWindow. The per-stream peak memory
// bound is therefore roughly StreamWindow + MaxMessageSize, not StreamWindow.
// This amplification is deliberate: it decouples the backpressure window (kept
// small for tight, consume-driven flow control) from the occasional large
// message, and the overdraft is transient -- repaid as soon as the message is
// consumed -- so steady-state memory stays near the window. Keep MaxMessageSize
// only as large as the largest expected message to bound the amplification.
type FlowControl struct {
	// Enabled turns per-stream flow control on.
	Enabled bool

	// StreamWindow is the sender's initial and nominal per-stream credit.
	StreamWindow int64

	// GrantThreshold is the consumed-byte credit that must accrue before the
	// receive side emits a grant, coalescing many consumes into one
	// KindWindowUpdate.
	GrantThreshold int64

	// MaxMessageSize bounds a single message's wire size. A send larger than
	// this fails fast rather than deadlocking on credit it can never repay, and
	// the receiver rejects an assembling message that exceeds it. It also caps
	// the overdraft: to finish a message the sender may exceed StreamWindow, but
	// only up to this size. Must be positive; a bound smaller than the window is
	// allowed and simply bounds transient memory more tightly.
	MaxMessageSize int64
}

// Default flow-control sizes, applied by SetDefaults to recover from an invalid
// configuration.
const (
	defaultStreamWindow   = 2 << 20   // 2 MiB
	defaultGrantThreshold = 512 << 10 // 512 KiB (a quarter of the default window)

	// DefaultMaxMessageSize is applied when flow control is enabled without a
	// message bound. It is exported because the install site defaults it
	// independently (an omitted bound should not force window/threshold defaults).
	DefaultMaxMessageSize = 64 << 20 // 64 MiB
)

// SetDefaults resets the flow-control sizes to their default values. The
// installation site calls it to recover from an invalid configuration instead of
// failing.
func (fc *FlowControl) SetDefaults() {
	fc.StreamWindow = defaultStreamWindow
	fc.GrantThreshold = defaultGrantThreshold
	fc.MaxMessageSize = DefaultMaxMessageSize
}

// GetStreamFlowControl returns the FlowControl stored in the options.
func GetStreamFlowControl(opts *Stream) FlowControl { return opts.flowControl }

// SetStreamFlowControl sets the FlowControl stored in the options.
func SetStreamFlowControl(opts *Stream, fc FlowControl) { opts.flowControl = fc }

// GetStreamTransport returns the drpc.Transport stored in the options.
func GetStreamTransport(opts *Stream) drpc.Transport { return opts.transport }

// SetStreamTransport sets the drpc.Transport stored in the options.
func SetStreamTransport(opts *Stream, tr drpc.Transport) { opts.transport = tr }

// GetStreamKind returns the StreamKind stored in the options.
func GetStreamKind(opts *Stream) drpc.StreamKind { return opts.kind }

// SetStreamKind sets the StreamKind stored in the options.
func SetStreamKind(opts *Stream, kind drpc.StreamKind) { opts.kind = kind }

// GetStreamRPC returns the RPC debug string stored in the options.
func GetStreamRPC(opts *Stream) string { return opts.rpc }

// SetStreamRPC sets the RPC debug string stored in the options.
func SetStreamRPC(opts *Stream, rpc string) { opts.rpc = rpc }

// GetStreamStats returns the Stats stored in the options.
func GetStreamStats(opts *Stream) *drpcstats.Stats { return opts.stats }

// SetStreamStats sets the Stats stored in the options.
func SetStreamStats(opts *Stream, stats *drpcstats.Stats) { opts.stats = stats }
