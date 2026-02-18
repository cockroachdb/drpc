// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcwire"
)

func TestSharedWriteBuf_AppendDrain(t *testing.T) {
	sw := newSharedWriteBuf()

	fr := drpcwire.Frame{
		Data: []byte("hello"),
		ID:   drpcwire.ID{Stream: 1, Message: 2},
		Kind: drpcwire.KindMessage,
		Done: true,
	}

	assert.NoError(t, sw.Append(fr))

	// Drain should return serialized bytes.
	data := sw.Drain(nil)
	assert.That(t, len(data) > 0)

	// Parse the frame back out to verify correctness.
	_, got, ok, err := drpcwire.ParseFrame(data)
	assert.NoError(t, err)
	assert.That(t, ok)
	assert.DeepEqual(t, got.Data, fr.Data)
	assert.Equal(t, got.ID.Stream, fr.ID.Stream)
	assert.Equal(t, got.ID.Message, fr.ID.Message)
	assert.Equal(t, got.Kind, fr.Kind)
	assert.Equal(t, got.Done, fr.Done)
}

func TestSharedWriteBuf_CloseIdempotent(t *testing.T) {
	sw := newSharedWriteBuf()
	sw.Close()
	sw.Close() // must not panic
}

func TestSharedWriteBuf_AppendAfterClose(t *testing.T) {
	sw := newSharedWriteBuf()
	sw.Close()

	err := sw.Append(drpcwire.Frame{})
	assert.Error(t, err)
}

func TestSharedWriteBuf_WaitAndDrainBlocks(t *testing.T) {
	sw := newSharedWriteBuf()

	done := make(chan struct{})
	go func() {
		defer close(done)
		data, ok := sw.WaitAndDrain(nil)
		assert.That(t, ok)
		assert.That(t, len(data) > 0)
	}()

	// Append should wake the blocked WaitAndDrain.
	assert.NoError(t, sw.Append(drpcwire.Frame{Data: []byte("a")}))
	<-done
}

func TestSharedWriteBuf_WaitAndDrainCloseEmpty(t *testing.T) {
	sw := newSharedWriteBuf()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, ok := sw.WaitAndDrain(nil)
		assert.That(t, !ok)
	}()

	// Close on empty buffer should return ok=false.
	sw.Close()
	<-done
}
