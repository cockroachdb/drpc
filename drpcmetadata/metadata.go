// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmetadata

import (
	"context"
	"strings"

	"github.com/zeebo/errs"
)

// AddPairs attaches metadata onto a context and return the context.
func AddPairs(ctx context.Context, metadata map[string]string) context.Context {
	// Get returns a copy of metadata
	newMetadata, ok := Get(ctx)
	if !ok {
		newMetadata = make(map[string]string)
	}
	for k, v := range metadata {
		newMetadata[strings.ToLower(k)] = v
	}
	return context.WithValue(ctx, metadataKey{}, newMetadata)
}

// NewIncomingContext attaches new metadata onto a context and returns the
// context.
func NewIncomingContext(ctx context.Context,
	metadata map[string]string) context.Context {
	newMetadata := make(map[string]string)
	for k, v := range metadata {
		newMetadata[strings.ToLower(k)] = v
	}
	return context.WithValue(ctx, metadataKey{}, newMetadata)
}

// Encode generates byte form of the metadata and appends it onto the passed in buffer.
func Encode(buf []byte, metadata map[string]string) ([]byte, error) {
	for key, value := range metadata {
		buf = appendEntry(buf, key, value)
	}
	return buf, nil
}

// Decode translate byte form of metadata into key/value metadata.
func Decode(buf []byte) (map[string]string, error) {
	var out map[string]string
	var key, value []byte
	var ok bool
	var err error

	for len(buf) > 0 {
		buf, key, value, ok, err = readEntry(buf)
		if err != nil {
			return nil, err
		} else if !ok {
			return nil, errs.New("invalid data")
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[string(key)] = string(value)
	}

	return out, nil
}

type metadataKey struct{}

// ClearContext removes all metadata from the context and returns a new context
// with no metadata attached.
func ClearContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, metadataKey{}, nil)
}

// ClearContextExcept removes all metadata from the context except for the
// specified key. If the specified key doesn't exist in the metadata, it clears
// all metadata. Returns a new context with only the specified key-value pair
// preserved.
func ClearContextExcept(ctx context.Context, key string) context.Context {
	value, ok := GetValue(ctx, key)
	if !ok {
		return ClearContext(ctx)
	}
	return context.WithValue(ctx, metadataKey{},
		map[string]string{strings.ToLower(key): value})
}

// Add associates a key/value pair on the context.
func Add(ctx context.Context, key, value string) context.Context {
	// Get returns a copy of metadata
	metadata, ok := Get(ctx)
	if !ok {
		metadata = make(map[string]string)
	}
	metadata[strings.ToLower(key)] = value
	return context.WithValue(ctx, metadataKey{}, metadata)
}

// Get returns all key/value pairs on the given context.
func Get(ctx context.Context) (map[string]string, bool) {
	metadata, ok := ctx.Value(metadataKey{}).(map[string]string)
	if !ok {
		return nil, false
	}
	// Return a copy to prevent mutation of the original map
	copy := make(map[string]string)
	for k, v := range metadata {
		copy[k] = v
	}
	return copy, true
}

// GetValue retrieves a specific value by key from the context's metadata.
// The input key is assumed to be lowercase. If metadata was created using
// the provided helper functions (Add, AddPairs, NewIncomingContext, etc.),
// this performs a fast O(1) lookup. If metadata was attached externally with
// mixed-case keys, a slower fallback search is performed.
func GetValue(ctx context.Context, key string) (string, bool) {
	metadata, ok := ctx.Value(metadataKey{}).(map[string]string)
	if !ok {
		return "", false
	}
	if val, ok := metadata[key]; ok {
		return val, true
	}
	// TODO: Check if we really need this. Keeping this for now, to conform to
	// grpc metadata semantics
	for k, v := range metadata {
		// We need to manually convert all keys to lower case,
		// because metadata is a map,
		//and there's no guarantee that the metadata
		// attached to the context is created using our helper functions.
		if len(k) == len(key) && strings.ToLower(k) == key {
			return v, true
		}
	}
	return "", false
}

// FastFromIncomingContext is a specialization of Get() and is
// based on grpcutil.FastFromIncomingContext from the cockroach repo.
// It extracts the metadata from the context, if any, by reference.
//
//   - Unlike Get, this variant does not guarantee that all the metadata keys
//     are lowercase.
//   - The caller promises to not modify the returned metadata -- the dRPC
//     APIs assume that the map in the context remains constant.
func FastFromIncomingContext(ctx context.Context) (map[string]string, bool) {
	metadata, ok := ctx.Value(metadataKey{}).(map[string]string)
	if !ok {
		return nil, false
	}
	return metadata, true
}

// NewOutgoingContext attaches new metadata onto an outgoing context and returns
// the context. Same as NewIncomingContext for now,
// as we don't have separate keys for incoming and outgoing metadata.
// Will be fixed as part https://github.com/cockroachdb/cockroach/i`ssues/156444
func NewOutgoingContext(ctx context.Context,
	metadata map[string]string) context.Context {
	newMetadata := make(map[string]string)
	for k, v := range metadata {
		newMetadata[strings.ToLower(k)] = v
	}
	return context.WithValue(ctx, metadataKey{}, newMetadata)
}
