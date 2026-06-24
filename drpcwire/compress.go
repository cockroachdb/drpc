// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"github.com/golang/snappy"

	"storj.io/drpc"
)

// CompressionMetadataKey is the metadata key used to signal the compression
// algorithm from client to server during stream invocation.
const CompressionMetadataKey = "drpc-compression"

// CompressionName returns the wire-protocol name for the compression algorithm.
// Returns "" for CompressionNone.
func CompressionName(c drpc.Compression) string {
	switch c {
	case drpc.CompressionSnappy:
		return "snappy"
	default:
		return ""
	}
}

// Compress returns the compressed form of src using the given algorithm.
// dst is used as scratch space when it has sufficient capacity.
// For CompressionNone, src is returned directly.
func Compress(c drpc.Compression, dst, src []byte) []byte {
	switch c {
	case drpc.CompressionSnappy:
		// Reset length to capacity so snappy.Encode can reuse the buffer.
		return snappy.Encode(dst[:cap(dst)], src)
	default:
		return src
	}
}

// Decompress returns the decompressed form of src using the given algorithm.
// dst is used as scratch space when it has sufficient capacity.
// For CompressionNone, src is returned directly.
func Decompress(c drpc.Compression, dst, src []byte) ([]byte, error) {
	switch c {
	case drpc.CompressionSnappy:
		// Reset length to capacity so snappy.Decode can reuse the buffer.
		return snappy.Decode(dst[:cap(dst)], src)
	default:
		return src, nil
	}
}

// CompressionFromName returns the Compression for the given wire name.
// It returns (CompressionNone, false) if the name is not recognized.
func CompressionFromName(name string) (drpc.Compression, bool) {
	switch name {
	case "snappy":
		return drpc.CompressionSnappy, true
	default:
		return drpc.CompressionNone, false
	}
}
