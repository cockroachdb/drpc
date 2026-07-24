package drpcwire

import (
	"storj.io/drpc"
)

// PacketAssembler assembles frames into complete packets, enforcing wire
// protocol invariants:
//   - All frames must belong to the same stream ID (set explicitly via
//     SetStreamID, or inferred from the first frame).
//   - Message IDs must be monotonically increasing.
//   - Frame kind must not change within a single packet (multi-frame).
//
// It is not safe for concurrent use.
type PacketAssembler struct {
	pk                Packet
	assembling        bool
	streamInitialized bool

	discardedLen int
}

// NewPacketAssembler returns a new PacketAssembler ready to assemble frames.
func NewPacketAssembler() PacketAssembler {
	return PacketAssembler{
		pk: Packet{
			ID: ID{Stream: 0, Message: 1},
		},
	}
}

// SetStreamID sets the expected stream ID. Frames for a different stream will
// be rejected. If not called, the stream ID is inferred from the first frame.
func (pa *PacketAssembler) SetStreamID(streamID uint64) {
	pa.pk.ID.Stream = streamID
	pa.streamInitialized = true
}

// Reset clears all assembly state, preparing the assembler for a new stream.
func (pa *PacketAssembler) Reset() {
	pa.pk = Packet{
		ID: ID{Stream: 0, Message: 1},
	}
	pa.assembling = false
	pa.streamInitialized = false
	pa.discardedLen = 0
}

// DiscardedBytes returns the payload byte count of an unfinished KindMessage
// dropped by the most recent AppendFrame (a higher message id superseded it),
// and clears the record. n is 0 when nothing was dropped. Only KindMessage
// partials are tracked, since only message payloads are byte-accounted by the
// receive window.
func (pa *PacketAssembler) DiscardedBytes() (n int) {
	n = pa.discardedLen
	pa.discardedLen = 0
	return n
}

// AppendFrame adds a frame to the in-progress packet. It returns the completed
// packet and true when a frame with Done=true is received. It returns false
// when more frames are needed to complete the packet.
func (pa *PacketAssembler) AppendFrame(fr Frame) (packet Packet, packetReady bool, err error) {
	// A discard record is scoped to the most recent AppendFrame (see
	// DiscardedBytes); clear any stale one before possibly setting a fresh one.
	pa.discardedLen = 0

	// Enforce stream ID consistency: infer from first frame or reject mismatches.
	if !pa.streamInitialized {
		pa.pk.ID.Stream = fr.ID.Stream
		pa.streamInitialized = true
	} else if fr.ID.Stream != pa.pk.ID.Stream {
		return Packet{}, false, drpc.ProtocolError.New(
			"frame stream mismatch: got stream %d, expected %d", fr.ID.Stream, pa.pk.ID.Stream)
	}

	if fr.ID.Message < pa.pk.ID.Message {
		return Packet{}, false, drpc.ProtocolError.New(
			"message id monotonicity violation: got %v, expected >= %v", fr.ID.Message, pa.pk.ID.Message)
	} else if fr.ID.Message > pa.pk.ID.Message || !pa.assembling {
		// New message: reset and start assembling. Record dropped unfinished
		// KindMessage bytes so byte-accounting callers can release them; other
		// kinds are not byte-accounted, so they are not tracked.
		if pa.assembling && len(pa.pk.Data) > 0 && pa.pk.Kind == KindMessage {
			pa.discardedLen = len(pa.pk.Data)
		}
		pa.pk.Data = pa.pk.Data[:0]
		pa.assembling = true
		pa.pk.ID.Message = fr.ID.Message
	} else if fr.Kind != pa.pk.Kind {
		return Packet{}, false, drpc.ProtocolError.New(
			"frame kind changed mid-packet: got %v, expected %v", fr.Kind, pa.pk.Kind)
	}

	// TODO(shubham): add buf reuse
	pa.pk.Data = append(pa.pk.Data, fr.Data...)
	pa.pk.Kind = fr.Kind
	pa.pk.Control = fr.Control

	if !fr.Done {
		return Packet{}, false, nil
	}

	packet = pa.pk

	pa.assembling = false
	pa.pk.ID.Message = fr.ID.Message + 1
	// Reuse the backing array: the caller must consume packet.Data before the
	// next AppendFrame call, as it will be overwritten.
	pa.pk.Data = pa.pk.Data[:0]
	return packet, true, nil
}
