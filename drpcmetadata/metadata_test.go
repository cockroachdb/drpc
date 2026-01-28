// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmetadata

import (
	"context"
	"testing"

	"github.com/zeebo/assert"
)

func TestGetFromIncomingContext(t *testing.T) {
	ctx := context.Background()

	metadata, ok := GetFromIncomingContext(ctx)
	assert.That(t, !ok)
	assert.Nil(t, metadata)

	ctx = NewIncomingContext(ctx, map[string]string{"ak": "av", "bk": "bv"})

	metadata, ok = GetFromIncomingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, metadata, map[string]string{
		"ak": "av",
		"bk": "bv",
	})
}

func TestGetFromOutgoingContext(t *testing.T) {
	ctx := context.Background()

	md, ok := GetFromOutgoingContext(ctx)
	assert.That(t, !ok)
	assert.Nil(t, md)

	ctx = context.WithValue(ctx, outgoingMetadataKey{},
		map[string]string{"existing": "value"})

	originalMd, ok := GetFromOutgoingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, originalMd, map[string]string{
		"existing": "value",
	})

	ctx = AppendToOutgoingContext(ctx, map[string]string{
		"foo": "bar",
		"ak":  "av",
		"bk":  "bv",
	})

	originalMd["existing"] = "modified"

	newMd, ok := GetFromOutgoingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, newMd, map[string]string{
		"existing": "value",
		"foo":      "bar",
		"ak":       "av",
		"bk":       "bv",
	})
}

func TestEncode(t *testing.T) {
	t.Run("Empty Metadata", func(t *testing.T) {
		var metadata map[string]string
		buf, err := Encode(nil, metadata)
		assert.Nil(t, buf)
		assert.NoError(t, err)
	})

	t.Run("With Metadata", func(t *testing.T) {
		data, err := Encode(nil, map[string]string{
			"test1": "a",
			"test2": "b",
		})
		assert.NoError(t, err)
		assert.That(t, len(data) > 0)
	})
}

func TestDecode(t *testing.T) {
	t.Run("Empty Metadata", func(t *testing.T) {
		metadata, err := Decode(nil)
		assert.NoError(t, err)
		assert.Nil(t, metadata)
	})

	t.Run("With Metadata", func(t *testing.T) {
		data := []byte{0xa, 0x9, 0xa, 0x4, 0x74, 0x65, 0x73, 0x74, 0x12, 0x1, 0x61}
		metadata, err := Decode(data)
		assert.NoError(t, err)
		assert.DeepEqual(t, metadata, map[string]string{"test": "a"})
	})
}

func TestMetadataImmutability(t *testing.T) {
	ctx := context.Background()
	ctx = NewIncomingContext(ctx, map[string]string{"foo": "bar"})

	metadata1, ok := GetFromIncomingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, metadata1["foo"], "bar")

	metadata1["foo"] = "modified"
	metadata1["new"] = "value"

	metadata2, ok := GetFromIncomingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, metadata2["foo"], "bar")
	assert.Equal(t, len(metadata2), 1)
}

func TestAppendToOutgoingContextImmutability(t *testing.T) {
	ctx := context.Background()
	ctx = NewOutgoingContext(ctx, map[string]string{"existing": "value"})

	originalCtx := ctx
	newCtx := AppendToOutgoingContext(ctx, map[string]string{
		"key1": "val1",
		"key2": "val2",
	})

	originalMd, ok := GetFromOutgoingContext(originalCtx)
	assert.That(t, ok)
	assert.Equal(t, len(originalMd), 1)
	assert.Equal(t, originalMd["existing"], "value")

	newMd, ok := GetFromOutgoingContext(newCtx)
	assert.That(t, ok)
	assert.Equal(t, len(newMd), 3)
	assert.Equal(t, newMd["existing"], "value")
	assert.Equal(t, newMd["key1"], "val1")
	assert.Equal(t, newMd["key2"], "val2")
}

func TestAppendToOutgoingContext(t *testing.T) {
	ctx := AppendToOutgoingContext(context.Background(), map[string]string{
		"key": "value",
	})

	md, ok := GetFromOutgoingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, md, map[string]string{
		"key": "value",
	})

	// Check that no incoming metadata was added
	incomingMd, ok := GetFromIncomingContext(ctx)
	assert.That(t, !ok)
	assert.Nil(t, incomingMd)

	newCtx := AppendToOutgoingContext(ctx, map[string]string{
		"key":     "modified",
		"new-key": "new-value",
	})

	newMd, ok := GetFromOutgoingContext(newCtx)
	assert.That(t, ok)
	assert.Equal(t, newMd, map[string]string{
		"key":     "modified",
		"new-key": "new-value",
	})

	// Check that incoming metadata is intact
	incomingMd, ok = GetFromIncomingContext(newCtx)
	assert.That(t, !ok)
	assert.Nil(t, incomingMd)
}

func TestNewIncomingContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), incomingMetadataKey{},
		map[string]string{
			"existing1": "value1",
			"existing2": "value2",
		})

	newCtx := NewIncomingContext(ctx, map[string]string{
		"existing1": "modified1",
		"key1":      "value1",
	})

	originalMd, ok := GetFromIncomingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, originalMd, map[string]string{
		"existing1": "value1",
		"existing2": "value2",
	})

	newMd, ok := GetFromIncomingContext(newCtx)
	assert.That(t, ok)
	assert.Equal(t, newMd, map[string]string{
		"existing1": "modified1",
		"key1":      "value1",
	})
}

func TestClearIncomingContext(t *testing.T) {
	ctx := context.Background()
	ctx = NewIncomingContext(ctx, map[string]string{"existing": "value"})

	ctx = ClearIncomingContext(ctx)
	newMd, ok := GetFromIncomingContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, newMd, map[string]string(nil))
}

func TestClearIncomingContextExcept(t *testing.T) {
	ctx := context.Background()
	ctx = NewIncomingContext(ctx, map[string]string{
		"key1": "value1", "key2": "value2",
	})

	ctx = ClearIncomingContextExcept(ctx, "key1")
	md, ok := GetFromIncomingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, md, map[string]string{
		"key1": "value1",
	})

	ctx = ClearIncomingContextExcept(ctx, "non-existent-key")
	md, ok = GetFromIncomingContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, md, map[string]string(nil))
}

func TestGetValueFromIncomingContext(t *testing.T) {
	ctx := context.Background()

	ctx = NewIncomingContext(ctx, map[string]string{
		"key1": "value1", "key2": "value2",
	})

	val, ok := GetValueFromIncomingContext(ctx, "non-existent-key")
	assert.False(t, ok)
	assert.Equal(t, val, "")

	val, ok = GetValueFromIncomingContext(ctx, "key1")
	assert.That(t, ok)
	assert.Equal(t, val, "value1")

	val, ok = GetValueFromIncomingContext(ctx, "key2")
	assert.That(t, ok)
	assert.Equal(t, val, "value2")

	val, ok = GetValueFromIncomingContext(ctx, "Key1") // case-sensitivity
	assert.False(t, ok)
	assert.Equal(t, val, "")
}

func TestNewOutgoingContext(t *testing.T) {
	ctx := context.Background()

	ctx = context.WithValue(ctx, outgoingMetadataKey{},
		map[string]string{"existing-key1": "existing-value1", "existing-key2": "existing-value2"})

	newCtx := NewOutgoingContext(ctx, map[string]string{
		"existing-key1": "new-value1",
		"key2":          "value2",
	})

	originalMd, ok := GetFromOutgoingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, originalMd, map[string]string{
		"existing-key1": "existing-value1",
		"existing-key2": "existing-value2",
	})

	newMd, ok := GetFromOutgoingContext(newCtx)
	assert.That(t, ok)
	assert.Equal(t, newMd, map[string]string{
		"existing-key1": "new-value1",
		"key2":          "value2",
	})
}
