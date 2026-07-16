// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
)

func testMuxWriter(t *testing.T) *drpcwire.MuxWriter {
	mw := drpcwire.NewMuxWriter(io.Discard, func(error) {})
	t.Cleanup(func() { mw.Stop(nil); <-mw.Done() })
	return mw
}

func testStream(t *testing.T, id uint64) *drpcstream.Stream {
	return drpcstream.New(context.Background(), id, testMuxWriter(t), drpcstream.NewBufferPool())
}

func TestActiveStreams_AddAndGet(t *testing.T) {
	streams := newActiveStreams()
	s := testStream(t, 1)

	assert.NoError(t, streams.Add(1, s, nil))

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
	s := testStream(t, 1)

	assert.NoError(t, streams.Add(1, s, nil))
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
	s1 := testStream(t, 1)
	s2 := testStream(t, 1)

	assert.NoError(t, streams.Add(1, s1, nil))
	assert.Error(t, streams.Add(1, s2, nil))

	// original stream is still present
	got, ok := streams.Get(1)
	assert.That(t, ok)
	assert.Equal(t, got, s1)
}

func TestActiveStreams_AddAfterClose(t *testing.T) {
	streams := newActiveStreams()
	streams.Close(errors.New("closed"))

	err := streams.Add(1, testStream(t, 1), nil)
	assert.Error(t, err)
}

func TestActiveStreams_RemoveAfterClose(t *testing.T) {
	streams := newActiveStreams()
	s := testStream(t, 1)
	assert.NoError(t, streams.Add(1, s, nil))

	streams.Close(errors.New("closed"))

	// must not panic
	streams.Remove(1)
}

func TestActiveStreams_Len(t *testing.T) {
	streams := newActiveStreams()
	assert.Equal(t, streams.Len(), 0)

	assert.NoError(t, streams.Add(1, testStream(t, 1), nil))
	assert.Equal(t, streams.Len(), 1)

	assert.NoError(t, streams.Add(2, testStream(t, 2), nil))
	assert.Equal(t, streams.Len(), 2)

	streams.Remove(1)
	assert.Equal(t, streams.Len(), 1)
}
