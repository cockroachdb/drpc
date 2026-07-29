// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"fmt"

	"github.com/golang/snappy"
	"github.com/minio/minlz"

	"storj.io/drpc"
)

// CompressionMetadataKey is the metadata key used to signal the compression
// algorithm from client to server during stream invocation.
const CompressionMetadataKey = "drpc-compression"

// minlzSnappyThreshold is the message size at or below which minlz falls back
// to snappy encoding. Benchmarks show snappy is faster for small payloads.
const minlzSnappyThreshold = 4 << 10 // 4 KiB

// minlzCompress adapts minlz.Encode to the error-free Compress signature.
// Messages at or below minlzSnappyThreshold or larger than minlz.MaxBlockSize
// (8MiB) are compressed with snappy instead; minlz.Decode transparently
// handles snappy-encoded blocks.
func minlzCompress(dst, src []byte, level int) []byte {
	if len(src) <= minlzSnappyThreshold || len(src) > minlz.MaxBlockSize {
		return snappy.Encode(dst, src)
	}
	buf, err := minlz.Encode(dst, src, level)
	if err != nil {
		panic(fmt.Sprintf("drpcwire: minlz encode of %d bytes: %+v", len(src), err))
	}
	return buf
}

// CompressionName returns the wire-protocol name for the compression variant.
// Returns "" for CompressionNone.
func CompressionName(c drpc.Compression) string {
	switch c {
	case drpc.CompressionSnappy:
		return "snappy"
	case drpc.CompressionMinLZFastest:
		return "minlz-fastest"
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
	case drpc.CompressionMinLZFastest:
		return minlzCompress(dst[:cap(dst)], src, minlz.LevelFastest)
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
	case drpc.CompressionMinLZFastest:
		return minlz.Decode(dst[:cap(dst)], src)
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
	case "minlz-fastest":
		return drpc.CompressionMinLZFastest, true
	default:
		return drpc.CompressionNone, false
	}
}
