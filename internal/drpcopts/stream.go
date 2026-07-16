// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcopts

import (
	"storj.io/drpc"
	"storj.io/drpc/drpcstats"
)

// Stream contains internal options for the drpcstream package.
type Stream struct {
	transport             drpc.Transport
	fin                   chan<- struct{}
	kind                  drpc.StreamKind
	rpc                   string
	stats                 *drpcstats.Stats
	onReceiveQueueEnqueue func(int64)
	onReceiveQueueDequeue func(int64)
	flowControl           FlowControl
}

// FlowControl configures per-stream flow control. Internal-only until the
// CockroachDB version-gated enablement is ready; a later change promotes it to
// a public option. The installation site validates it (see drpcstream).
type FlowControl struct {
	// Enabled turns per-stream flow control on.
	Enabled bool

	// StreamWindow is the sender's initial and nominal per-stream credit.
	StreamWindow int64

	// HighWater is the receive-side buffered-byte mark at or above which
	// grants are withheld.
	HighWater int64

	// GrantThreshold is the credit that must accrue before a grant is emitted,
	// coalescing many frames into one KindWindowUpdate.
	GrantThreshold int64
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

// GetStreamOnReceiveQueueEnqueue returns the receive queue enqueue hook.
func GetStreamOnReceiveQueueEnqueue(opts *Stream) func(int64) {
	return opts.onReceiveQueueEnqueue
}

// SetStreamOnReceiveQueueEnqueue sets the receive queue enqueue hook.
func SetStreamOnReceiveQueueEnqueue(opts *Stream, fn func(int64)) {
	opts.onReceiveQueueEnqueue = fn
}

// GetStreamOnReceiveQueueDequeue returns the receive queue dequeue hook.
func GetStreamOnReceiveQueueDequeue(opts *Stream) func(int64) {
	return opts.onReceiveQueueDequeue
}

// SetStreamOnReceiveQueueDequeue sets the receive queue dequeue hook.
func SetStreamOnReceiveQueueDequeue(opts *Stream, fn func(int64)) {
	opts.onReceiveQueueDequeue = fn
}
