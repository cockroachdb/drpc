// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
)

func testStream(id uint64) *drpcstream.Stream {
	return drpcstream.New(context.Background(), id, &drpcwire.Writer{})
}

func TestStreamRegistry_RegisterAndGet(t *testing.T) {
	reg := newStreamRegistry()
	s := testStream(1)

	assert.NoError(t, reg.Register(1, s))

	got, ok := reg.Get(1)
	assert.That(t, ok)
	assert.Equal(t, got, s)
}

func TestStreamRegistry_GetMissing(t *testing.T) {
	reg := newStreamRegistry()

	got, ok := reg.Get(42)
	assert.That(t, !ok)
	assert.Nil(t, got)
}

func TestStreamRegistry_Unregister(t *testing.T) {
	reg := newStreamRegistry()
	s := testStream(1)

	assert.NoError(t, reg.Register(1, s))
	assert.Equal(t, reg.Len(), 1)

	reg.Unregister(1)

	_, ok := reg.Get(1)
	assert.That(t, !ok)
	assert.Equal(t, reg.Len(), 0)
}

func TestStreamRegistry_UnregisterIdempotent(t *testing.T) {
	reg := newStreamRegistry()

	// must not panic when unregistering a non-existent ID
	reg.Unregister(99)
}

func TestStreamRegistry_DuplicateRegister(t *testing.T) {
	reg := newStreamRegistry()
	s1 := testStream(1)
	s2 := testStream(1)

	assert.NoError(t, reg.Register(1, s1))
	assert.Error(t, reg.Register(1, s2))

	// original stream is still registered
	got, ok := reg.Get(1)
	assert.That(t, ok)
	assert.Equal(t, got, s1)
}

func TestStreamRegistry_RegisterAfterClose(t *testing.T) {
	reg := newStreamRegistry()
	reg.Close()

	err := reg.Register(1, testStream(1))
	assert.Error(t, err)
}

func TestStreamRegistry_UnregisterAfterClose(t *testing.T) {
	reg := newStreamRegistry()
	s := testStream(1)
	assert.NoError(t, reg.Register(1, s))

	reg.Close()

	// must not panic
	reg.Unregister(1)
}

func TestStreamRegistry_Len(t *testing.T) {
	reg := newStreamRegistry()
	assert.Equal(t, reg.Len(), 0)

	assert.NoError(t, reg.Register(1, testStream(1)))
	assert.Equal(t, reg.Len(), 1)

	assert.NoError(t, reg.Register(2, testStream(2)))
	assert.Equal(t, reg.Len(), 2)

	reg.Unregister(1)
	assert.Equal(t, reg.Len(), 1)
}

func TestStreamRegistry_ForEach(t *testing.T) {
	reg := newStreamRegistry()
	s1 := testStream(1)
	s2 := testStream(2)
	s3 := testStream(3)

	assert.NoError(t, reg.Register(1, s1))
	assert.NoError(t, reg.Register(2, s2))
	assert.NoError(t, reg.Register(3, s3))

	seen := make(map[uint64]*drpcstream.Stream)
	reg.ForEach(func(s *drpcstream.Stream) {
		seen[s.ID()] = s
	})

	assert.Equal(t, len(seen), 3)
	assert.Equal(t, seen[1], s1)
	assert.Equal(t, seen[2], s2)
	assert.Equal(t, seen[3], s3)
}

func TestStreamRegistry_ForEach_Empty(t *testing.T) {
	reg := newStreamRegistry()

	count := 0
	reg.ForEach(func(_ *drpcstream.Stream) { count++ })
	assert.Equal(t, count, 0)
}
