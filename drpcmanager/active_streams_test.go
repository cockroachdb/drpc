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

func TestActiveStreams_AddAndGet(t *testing.T) {
	streams := newActiveStreams()
	s := testStream(1)

	assert.NoError(t, streams.Add(1, s))

	got, ok := streams.Get(1)
	assert.That(t, ok)
	assert.Equal(t, got, s)
}

func TestActiveStreams_GetMissing(t *testing.T) {
	streams := newActiveStreams()

	got, ok := streams.Get(42)
	assert.That(t, !ok)
	assert.Nil(t, got)
}

func TestActiveStreams_Remove(t *testing.T) {
	streams := newActiveStreams()
	s := testStream(1)

	assert.NoError(t, streams.Add(1, s))
	assert.Equal(t, streams.Len(), 1)

	streams.Remove(1)

	_, ok := streams.Get(1)
	assert.That(t, !ok)
	assert.Equal(t, streams.Len(), 0)
}

func TestActiveStreams_RemoveIdempotent(t *testing.T) {
	streams := newActiveStreams()

	// must not panic when removing a non-existent ID
	streams.Remove(99)
}

func TestActiveStreams_DuplicateAdd(t *testing.T) {
	streams := newActiveStreams()
	s1 := testStream(1)
	s2 := testStream(1)

	assert.NoError(t, streams.Add(1, s1))
	assert.Error(t, streams.Add(1, s2))

	// original stream is still present
	got, ok := streams.Get(1)
	assert.That(t, ok)
	assert.Equal(t, got, s1)
}

func TestActiveStreams_AddAfterClose(t *testing.T) {
	streams := newActiveStreams()
	streams.Close()

	err := streams.Add(1, testStream(1))
	assert.Error(t, err)
}

func TestActiveStreams_RemoveAfterClose(t *testing.T) {
	streams := newActiveStreams()
	s := testStream(1)
	assert.NoError(t, streams.Add(1, s))

	streams.Close()

	// must not panic
	streams.Remove(1)
}

func TestActiveStreams_Len(t *testing.T) {
	streams := newActiveStreams()
	assert.Equal(t, streams.Len(), 0)

	assert.NoError(t, streams.Add(1, testStream(1)))
	assert.Equal(t, streams.Len(), 1)

	assert.NoError(t, streams.Add(2, testStream(2)))
	assert.Equal(t, streams.Len(), 2)

	streams.Remove(1)
	assert.Equal(t, streams.Len(), 1)
}

func TestActiveStreams_ForEach(t *testing.T) {
	streams := newActiveStreams()
	s1 := testStream(1)
	s2 := testStream(2)
	s3 := testStream(3)

	assert.NoError(t, streams.Add(1, s1))
	assert.NoError(t, streams.Add(2, s2))
	assert.NoError(t, streams.Add(3, s3))

	seen := make(map[uint64]*drpcstream.Stream)
	streams.ForEach(func(s *drpcstream.Stream) {
		seen[s.ID()] = s
	})

	assert.Equal(t, len(seen), 3)
	assert.Equal(t, seen[1], s1)
	assert.Equal(t, seen[2], s2)
	assert.Equal(t, seen[3], s3)
}

func TestActiveStreams_ForEach_Empty(t *testing.T) {
	streams := newActiveStreams()

	count := 0
	streams.ForEach(func(_ *drpcstream.Stream) { count++ })
	assert.Equal(t, count, 0)
}
