// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmetadata

import (
	"context"

	"github.com/zeebo/errs"
)

// AppendToOutgoingContext attaches metadata onto an outgoing context and
// returns the context.
func AppendToOutgoingContext(ctx context.Context,
	metadata map[string]string) context.Context {
	// Get existing metadata
	newMetadata, ok := GetFromOutgoingContext(ctx)
	if !ok {
		newMetadata = make(map[string]string)
	}
	for k, v := range metadata {
		newMetadata[k] = v
	}
	return context.WithValue(ctx, outgoingMetadataKey{}, newMetadata)
}

// NewIncomingContext attaches new metadata onto a context and returns the
// context.
func NewIncomingContext(ctx context.Context,
	metadata map[string]string) context.Context {
	newMetadata := make(map[string]string)
	for k, v := range metadata {
		newMetadata[k] = v
	}
	return context.WithValue(ctx, incomingMetadataKey{}, newMetadata)
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

type incomingMetadataKey struct{}
type outgoingMetadataKey struct{}

// ClearIncomingContext removes all metadata from the incoming context and returns a new
// context with no incoming metadata attached.
func ClearIncomingContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, incomingMetadataKey{}, nil)
}

// ClearIncomingContextExcept removes all metadata from the incoming context
// except for the specified key. If the specified key doesn't exist in the metadata,
// it clears all metadata. Returns a new context with only the specified key-value pair
// preserved.
func ClearIncomingContextExcept(ctx context.Context,
	key string) context.Context {
	value, ok := GetValueFromIncomingContext(ctx, key)
	if !ok {
		return ClearIncomingContext(ctx)
	}
	return context.WithValue(ctx, incomingMetadataKey{},
		map[string]string{key: value})
}

// GetFromIncomingContext returns all key/value pairs on the given incoming context.
func GetFromIncomingContext(ctx context.Context) (map[string]string, bool) {
	metadata, ok := ctx.Value(incomingMetadataKey{}).(map[string]string)
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

// GetValueFromIncomingContext retrieves a specific value by key from the context's metadata.
func GetValueFromIncomingContext(ctx context.Context, key string) (string,
	bool) {
	metadata, ok := ctx.Value(incomingMetadataKey{}).(map[string]string)
	if !ok {
		return "", false
	}
	val, ok := metadata[key]
	return val, ok
}

// NewOutgoingContext attaches new metadata onto an outgoing context and returns
// the context.
func NewOutgoingContext(ctx context.Context,
	metadata map[string]string) context.Context {
	newMetadata := make(map[string]string)
	for k, v := range metadata {
		newMetadata[k] = v
	}
	return context.WithValue(ctx, outgoingMetadataKey{}, newMetadata)
}

// GetFromOutgoingContext returns all key/value pairs on the outgoing
// context.
func GetFromOutgoingContext(ctx context.Context) (map[string]string, bool) {
	// Get existing metadata
	existingMetadata, ok := ctx.Value(outgoingMetadataKey{}).(map[string]string)
	if !ok {
		return nil, false
	}
	newMetadata := make(map[string]string)
	for k, v := range existingMetadata {
		newMetadata[k] = v
	}
	return newMetadata, true
}
