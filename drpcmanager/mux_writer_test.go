// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"testing"

	"github.com/zeebo/assert"
	"storj.io/drpc/drpcwire"
)

func TestMuxWriter_WritePacket(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}

	pkt := drpcwire.Packet{
		Data: []byte("hello"),
		ID:   drpcwire.ID{Stream: 1, Message: 2},
		Kind: drpcwire.KindMessage,
	}

	assert.NoError(t, w.WritePacket(pkt))

	data := sw.Drain(nil)
	_, got, ok, err := drpcwire.ParseFrame(data)
	assert.NoError(t, err)
	assert.That(t, ok)
	assert.DeepEqual(t, got.Data, pkt.Data)
	assert.Equal(t, got.ID.Stream, pkt.ID.Stream)
	assert.Equal(t, got.ID.Message, pkt.ID.Message)
	assert.Equal(t, got.Kind, pkt.Kind)
	assert.Equal(t, got.Done, true)
}

func TestMuxWriter_WritePacketIsolatesData(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}

	data := []byte("hello")
	pkt := drpcwire.Packet{
		Data: data,
		ID:   drpcwire.ID{Stream: 1, Message: 2},
		Kind: drpcwire.KindMessage,
	}

	assert.NoError(t, w.WritePacket(pkt))

	// Mutate the original source buffer after WritePacket.
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

func TestMuxWriter_WritePacketAfterClose(t *testing.T) {
	sw := newSharedWriteBuf()
	w := &muxWriter{sw: sw}
	sw.Close()

	err := w.WritePacket(drpcwire.Packet{})
	assert.Error(t, err)
}
