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
	ctx = Add(ctx, "Akey", "Avalue")

	{
		metadata, ok := Get(ctx)
		assert.That(t, ok)
		assert.Equal(t, metadata, map[string]string{
			"foo":  "bar",
			"akey": "Avalue",
		})
	}

	ctx = AddPairs(ctx, map[string]string{
		"ak": "av",
		"bk": "bv",
		"Ck": "Cv",
	})

	{
		metadata, ok := Get(ctx)
		assert.That(t, ok)
		assert.Equal(t, metadata, map[string]string{
			"foo":  "bar",
			"akey": "Avalue",
			"ak":   "av",
			"bk":   "bv",
			"ck":   "Cv",
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

func TestNewIncomingContext(t *testing.T) {
	ctx := context.Background()

	ctx = Add(ctx, "existing", "value")

	ctx = NewIncomingContext(ctx, map[string]string{
		"key1": "value1",
		"Key2": "Value2",
	})
	md, ok := Get(ctx)
	assert.That(t, ok)
	assert.Equal(t, md, map[string]string{
		"key1": "value1",
		"key2": "Value2",
	})
}

func TestClearContext(t *testing.T) {
	ctx := context.Background()
	ctx = Add(ctx, "existing", "value")

	ctx = ClearContext(ctx)
	newMd, ok := Get(ctx)
	assert.False(t, ok)
	assert.Equal(t, newMd, map[string]string(nil))
}

func TestClearContextExcept(t *testing.T) {
	ctx := context.Background()
	ctx = AddPairs(ctx, map[string]string{
		"key1": "value1", "key2": "value2",
	})

	ctx = ClearContextExcept(ctx, "key1")
	md, ok := Get(ctx)
	assert.That(t, ok)
	assert.Equal(t, md, map[string]string{
		"key1": "value1",
	})

	ctx = ClearContextExcept(ctx, "non-existent-key")
	md, ok = Get(ctx)
	assert.False(t, ok)
	assert.Equal(t, md, map[string]string(nil))
}

func TestGetValue(t *testing.T) {
	ctx := context.Background()

	val, ok := GetValue(ctx, "non-existent-key")
	assert.False(t, ok)
	assert.Equal(t, val, "")

	ctx = context.WithValue(ctx, metadataKey{},
		map[string]string{
			"External-mixed-case-key": "value",
		})
	ctx = AddPairs(ctx, map[string]string{
		"key": "value1", "Mixed-case-key": "value2",
	})

	val, ok = GetValue(ctx, "external-mixed-case-key")
	assert.That(t, ok)
	assert.Equal(t, val, "value")

	val, ok = GetValue(ctx, "key")
	assert.That(t, ok)
	assert.Equal(t, val, "value1")

	val, ok = GetValue(ctx, "mixed-case-key")
	assert.That(t, ok)
	assert.Equal(t, val, "value2")

	val, ok = GetValue(ctx, "Mixed-case-key")
	assert.False(t, ok)
}

func TestFastFromIncomingContext(t *testing.T) {
	ctx := context.Background()

	// Add some metadata external to the package to test key normalization
	ctx = context.WithValue(ctx, metadataKey{},
		map[string]string{"External-mixed-case-key": "external-value"})

	ctx = Add(ctx, "key", "value")
	md, ok := FastFromIncomingContext(ctx)
	assert.That(t, ok)
	assert.Equal(t, md,
		map[string]string{"External-mixed-case-key": "external-value",
			"key": "value"})
}

func TestNewOutgoingContext(t *testing.T) {
	ctx := context.Background()

	ctx = Add(ctx, "existing", "value")

	ctx = NewOutgoingContext(ctx, map[string]string{
		"key1": "value1",
		"Key2": "Value2",
	})
	md, ok := Get(ctx)
	assert.That(t, ok)
	assert.Equal(t, md, map[string]string{
		"key1": "value1",
		"key2": "Value2",
	})
}
