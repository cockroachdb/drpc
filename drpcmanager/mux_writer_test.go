// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcwire"
)

func TestMuxWriter_WriteFrame(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}

	fr := drpcwire.Frame{
		Data: []byte("hello"),
		ID:   drpcwire.ID{Stream: 1, Message: 2},
		Kind: drpcwire.KindMessage,
		Done: true,
	}

	assert.NoError(t, w.WriteFrame(fr))

	data := sw.Drain(nil)
	_, got, ok, err := drpcwire.ParseFrame(data)
	assert.NoError(t, err)
	assert.That(t, ok)
	assert.DeepEqual(t, got.Data, fr.Data)
	assert.Equal(t, got.ID.Stream, fr.ID.Stream)
	assert.Equal(t, got.ID.Message, fr.ID.Message)
	assert.Equal(t, got.Kind, fr.Kind)
	assert.Equal(t, got.Done, fr.Done)
}

func TestMuxWriter_WriteFrameIsolatesData(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}

	data := []byte("hello")
	fr := drpcwire.Frame{
		Data: data,
		ID:   drpcwire.ID{Stream: 1, Message: 2},
		Kind: drpcwire.KindMessage,
		Done: true,
	}

	assert.NoError(t, w.WriteFrame(fr))

	// Mutate the original source buffer after WriteFrame.
	data[0] = 'j'

	// The serialized data in the shared buffer should be unaffected because
	// AppendFrame copies the bytes during serialization.
	buf := sw.Drain(nil)
	_, got, ok, err := drpcwire.ParseFrame(buf)
	assert.NoError(t, err)
	assert.That(t, ok)
	assert.DeepEqual(t, got.Data, []byte("hello"))
}

func TestMuxWriter_FlushNoop(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}
	assert.NoError(t, w.Flush())
}

func TestMuxWriter_Empty(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}
	assert.That(t, w.Empty())
}

func TestMuxWriter_WriteFrameAfterClose(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}
	sw.Close()

	err := w.WriteFrame(drpcwire.Frame{})
	assert.Error(t, err)
}
