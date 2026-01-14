// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmetadata

import (
	"context"
	"strings"

	"github.com/zeebo/errs"
)

// AddPairs attaches metadata onto an incoming context and returns the context.
func AddPairs(ctx context.Context, metadata map[string]string) context.Context {
	// Get returns a copy of metadata
	newMetadata, ok := Get(ctx)
	if !ok {
		newMetadata = make(map[string]string)
	}
	for k, v := range metadata {
		newMetadata[k] = v
	}
	return context.WithValue(ctx, incomingMetadataKey{}, newMetadata)
}

// AddPairsToOutgoingContext attaches metadata onto an outgoing context and
// returns the context.
func AddPairsToOutgoingContext(ctx context.Context, metadata map[string]string) context.Context {
	// Get existing metadata
	existingMd, ok := ctx.Value(outgoingMetadataKey{}).(map[string]string)
	if !ok {
		return ctx
	}
	newMetadata := make(map[string]string)
	for k, v := range existingMd {
		newMetadata[k] = v
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

// NewOutgoingContext attaches new metadata onto a context and returns the
// context.
func NewOutgoingContext(ctx context.Context,
	metadata map[string]string) context.Context {
	newMetadata := make(map[string]string)
	for k, v := range metadata {
		newMetadata[k] = v
	}
	return context.WithValue(ctx, outgoingMetadataKey{}, newMetadata)
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

// ClearContext removes all metadata from the incoming context and returns a new
// context with no metadata attached.
func ClearContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, incomingMetadataKey{}, nil)
}

// ClearContextExcept removes all metadata from the incoming context except for
// the specified key. If the specified key doesn't exist in the metadata, it clears
// all metadata. Returns a new context with only the specified key-value pair
// preserved.
func ClearContextExcept(ctx context.Context, key string) context.Context {
	value, ok := GetValue(ctx, key)
	if !ok {
		return ClearContext(ctx)
	}
	return context.WithValue(ctx, incomingMetadataKey{},
		map[string]string{strings.ToLower(key): value})
}

// Add associates a key/value pair on the incoming context.
func Add(ctx context.Context, key, value string) context.Context {
	// Get returns a copy of metadata
	metadata, ok := Get(ctx)
	if !ok {
		metadata = make(map[string]string)
	}
	metadata[key] = value
	return context.WithValue(ctx, incomingMetadataKey{}, metadata)
}

// Get returns all key/value pairs on the given incoming context.
func Get(ctx context.Context) (map[string]string, bool) {
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

// GetFromOutgoingContext returns all key/value pairs on the given incoming
// context.
func GetFromOutgoingContext(ctx context.Context) (map[string]string, bool) {
	metadata, ok := ctx.Value(outgoingMetadataKey{}).(map[string]string)
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

// GetValue retrieves a specific value by key from the incoming context's
// metadata.
func GetValue(ctx context.Context, key string) (string, bool) {
	metadata, ok := ctx.Value(incomingMetadataKey{}).(map[string]string)
	if !ok {
		return "", false
	}
	val, ok := metadata[key]
	return val, ok
}
