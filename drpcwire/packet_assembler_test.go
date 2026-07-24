// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"strings"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
)

func TestPacketAssembler_WrongStreamID(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	_, _, err := pa.AppendFrame(Frame{
		ID:   ID{Stream: 2, Message: 1},
		Kind: KindMessage,
		Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "frame stream mismatch"))
}

func TestPacketAssembler_StreamIDInferredFromFirstFrame(t *testing.T) {
	pa := NewPacketAssembler()

	// First frame sets the stream ID implicitly.
	_, _, err := pa.AppendFrame(Frame{
		ID:   ID{Stream: 5, Message: 1},
		Kind: KindMessage,
		Done: true,
	})
	assert.NoError(t, err)

	// Second frame for a different stream is rejected.
	_, _, err = pa.AppendFrame(Frame{
		ID:   ID{Stream: 6, Message: 2},
		Kind: KindMessage,
		Done: true,
	})
	assert.Error(t, err)
	assert.That(t, strings.Contains(err.Error(), "frame stream mismatch"))
}

// A frame with a message ID lower than a previously completed message is rejected.
func TestPacketAssembler_MessageMonotonicity(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	// m3 completes, next expected becomes 4.
	_, _, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 3}, Kind: KindMessage, Done: true,
	})
	assert.NoError(t, err)

	// m2 < 4 → error.
	_, _, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "monotonicity"))
}

// When a higher message ID arrives mid-assembly, the in-progress data is
// silently discarded and a new packet begins.
func TestPacketAssembler_HigherMsgDiscardsInProgress(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	// Start accumulating m1.
	_, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("discard"), Done: false,
	})
	assert.NoError(t, err)
	assert.That(t, !ready)

	// m2 arrives, m1 data should be silently discarded.
	pkt, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Data: []byte("kept"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)
	assert.DeepEqual(t, pkt.Data, []byte("kept"))

	// The dropped bytes are reported once so byte accounting can release them.
	n := pa.DiscardedBytes()
	assert.Equal(t, n, len("discard"))
	n = pa.DiscardedBytes()
	assert.Equal(t, n, 0)
}

// A completed message followed by the next message id is not a discard.
func TestPacketAssembler_CompletedMessageNotReportedDiscarded(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	_, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("done"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)

	_, ready, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Data: []byte("next"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)

	n := pa.DiscardedBytes()
	assert.Equal(t, n, 0)
}

// Discard state is scoped to the most recent AppendFrame: a later
// non-discarding append clears an undrained record, and Reset drops it too.
func TestPacketAssembler_DiscardStateNotStale(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	_, _, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("old"), Done: false,
	})
	assert.NoError(t, err)
	_, _, err = pa.AppendFrame(Frame{ // supersedes m1: discard recorded
		ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Data: []byte("a"), Done: false,
	})
	assert.NoError(t, err)
	_, _, err = pa.AppendFrame(Frame{ // non-discarding: undrained record cleared
		ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Data: []byte("b"), Done: true,
	})
	assert.NoError(t, err)
	n := pa.DiscardedBytes()
	assert.Equal(t, n, 0)

	// Reset clears a pending record as well.
	_, _, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 3}, Kind: KindMessage, Data: []byte("old"), Done: false,
	})
	assert.NoError(t, err)
	_, _, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 4}, Kind: KindMessage, Data: []byte("new"), Done: true,
	})
	assert.NoError(t, err)
	pa.Reset()
	n = pa.DiscardedBytes()
	assert.Equal(t, n, 0)
}

// Continuation frames (same message ID, mid-assembly) must carry the same
// kind as the first frame. A kind change mid-packet is a protocol error.
func TestPacketAssembler_KindChangeWithinPacket(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	_, _, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Done: false,
	})
	assert.NoError(t, err)

	_, _, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindError, Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "kind change"))
}

// Multiple continuation frames for the same message accumulate data correctly.
func TestPacketAssembler_MultiFrameDataAccumulation(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	_, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("hel"), Done: false,
	})
	assert.NoError(t, err)
	assert.That(t, !ready)

	_, ready, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("lo "), Done: false,
	})
	assert.NoError(t, err)
	assert.That(t, !ready)

	pkt, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("world"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)
	assert.DeepEqual(t, pkt.Data, []byte("hello world"))
}

// Multi-frame assembly works when the message ID is greater than the initial
// expected ID (e.g., on the server side where invoke consumed earlier message
// IDs). Continuation frames must accumulate data, not reset on each frame.
func TestPacketAssembler_MultiFrameWithSkippedMessageID(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	// msg=3 is greater than initial expected message ID=1.
	_, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 3}, Kind: KindMessage, Data: []byte("hel"), Done: false,
	})
	assert.NoError(t, err)
	assert.That(t, !ready)

	_, ready, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 3}, Kind: KindMessage, Data: []byte("lo"), Done: false,
	})
	assert.NoError(t, err)
	assert.That(t, !ready)

	pkt, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 3}, Kind: KindMessage, Data: []byte(" world"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)
	assert.DeepEqual(t, pkt.Data, []byte("hello world"))
}

// Once a message completes (done=true), the same message ID is rejected.
func TestPacketAssembler_DonePreventsReplay(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	// m1 completes → next expected becomes 2.
	_, _, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Done: true,
	})
	assert.NoError(t, err)

	// Same message ID again → error.
	_, _, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Done: true,
	})
	assert.Error(t, err)
	assert.That(t, drpc.ProtocolError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "monotonicity"))
}

// Kind consistency is only enforced within a packet (continuation frames), not
// across messages. A KindMessage followed by a KindClose for the next message
// should be accepted without error.
func TestPacketAssembler_KindChangeAcrossMessages(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	// Multi-frame message 1 with KindMessage.
	_, _, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("ab"), Done: false,
	})
	assert.NoError(t, err)

	pkt, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Data: []byte("cd"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)
	assert.DeepEqual(t, pkt.Data, []byte("abcd"))

	// Message 2 with a different kind — should not trigger kind check.
	pkt, ready, err = pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 2}, Kind: KindClose, Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)
	assert.Equal(t, pkt.Kind, KindClose)
}

// Reset clears all state so the assembler can be reused for a new stream.
func TestPacketAssembler_Reset(t *testing.T) {
	pa := NewPacketAssembler()
	pa.SetStreamID(1)

	// Complete a packet on stream 1.
	_, _, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Done: true,
	})
	assert.NoError(t, err)

	// After reset, stream ID is cleared and must be re-inferred.
	pa.Reset()

	// A frame for stream 2 should now be accepted.
	pkt, ready, err := pa.AppendFrame(Frame{
		ID: ID{Stream: 2, Message: 1}, Kind: KindMessage, Data: []byte("new"), Done: true,
	})
	assert.NoError(t, err)
	assert.That(t, ready)
	assert.DeepEqual(t, pkt.Data, []byte("new"))
	assert.Equal(t, pkt.ID.Stream, uint64(2))
}
