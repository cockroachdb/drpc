// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmetadata

import (
	"context"
	"testing"

	"github.com/zeebo/assert"
)

func TestAddGet(t *testing.T) {
	ctx := context.Background()

	{
		metadata, ok := Get(ctx)
		assert.That(t, !ok)
		assert.Nil(t, metadata)
	}

	ctx = Add(ctx, "foo", "bar")

	{
		metadata, ok := Get(ctx)
		assert.That(t, ok)
		assert.Equal(t, metadata, map[string]string{
			"foo": "bar",
		})
	}

	ctx = AddPairs(ctx, map[string]string{
		"ak": "av",
		"bk": "bv",
	})

	{
		metadata, ok := Get(ctx)
		assert.That(t, ok)
		assert.Equal(t, metadata, map[string]string{
			"foo": "bar",
			"ak":  "av",
			"bk":  "bv",
		})
	}
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
	ctx = Add(ctx, "foo", "bar")

	metadata1, ok := Get(ctx)
	assert.That(t, ok)
	assert.Equal(t, metadata1["foo"], "bar")

	metadata1["foo"] = "modified"
	metadata1["new"] = "value"

	metadata2, ok := Get(ctx)
	assert.That(t, ok)
	assert.Equal(t, metadata2["foo"], "bar")
	assert.Equal(t, len(metadata2), 1)
}

func TestAddImmutability(t *testing.T) {
	ctx := context.Background()
	ctx = Add(ctx, "original", "value")

	originalCtx := ctx
	newCtx := Add(ctx, "new", "key")

	originalMd, ok := Get(originalCtx)
	assert.That(t, ok)
	assert.Equal(t, len(originalMd), 1)
	assert.Equal(t, originalMd["original"], "value")

	newMd, ok := Get(newCtx)
	assert.That(t, ok)
	assert.Equal(t, len(newMd), 2)
	assert.Equal(t, newMd["original"], "value")
	assert.Equal(t, newMd["new"], "key")
}

func TestAddPairsImmutability(t *testing.T) {
	ctx := context.Background()
	ctx = Add(ctx, "existing", "value")

	originalCtx := ctx
	newCtx := AddPairs(ctx, map[string]string{
		"key1": "val1",
		"key2": "val2",
	})

	originalMd, ok := Get(originalCtx)
	assert.That(t, ok)
	assert.Equal(t, len(originalMd), 1)
	assert.Equal(t, originalMd["existing"], "value")

	newMd, ok := Get(newCtx)
	assert.That(t, ok)
	assert.Equal(t, len(newMd), 3)
	assert.Equal(t, newMd["existing"], "value")
	assert.Equal(t, newMd["key1"], "val1")
	assert.Equal(t, newMd["key2"], "val2")
}
