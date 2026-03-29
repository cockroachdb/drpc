// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcopts

import (
	"storj.io/drpc"
	"storj.io/drpc/drpcstats"
)

// Stream contains internal options for the drpcstream package.
type Stream struct {
	transport drpc.Transport
	fin       chan<- struct{}
	kind      drpc.StreamKind
	rpc       string
	stats     *drpcstats.Stats
}

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
