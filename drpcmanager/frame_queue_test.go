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

	pkt := drpcwire.Packet{
		Data: []byte("hello"),
		ID:   drpcwire.ID{Stream: 1, Message: 2},
		Kind: drpcwire.KindMessage,
	}

	assert.NoError(t, sw.Append(pkt))

	// Drain should return serialized bytes.
	data := sw.Drain(nil)
	assert.That(t, len(data) > 0)

	// Parse the frame back out to verify correctness.
	_, got, ok, err := drpcwire.ParseFrame(data)
	assert.NoError(t, err)
	assert.That(t, ok)
	assert.DeepEqual(t, got.Data, pkt.Data)
	assert.Equal(t, got.ID.Stream, pkt.ID.Stream)
	assert.Equal(t, got.ID.Message, pkt.ID.Message)
	assert.Equal(t, got.Kind, pkt.Kind)
	assert.Equal(t, got.Done, true)
}

func TestSharedWriteBuf_CloseIdempotent(t *testing.T) {
	sw := newSharedWriteBuf()
	sw.Close()
	sw.Close() // must not panic
}

func TestSharedWriteBuf_AppendAfterClose(t *testing.T) {
	sw := newSharedWriteBuf()
	sw.Close()

	err := sw.Append(drpcwire.Packet{})
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
	assert.NoError(t, sw.Append(drpcwire.Packet{Data: []byte("a")}))
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
