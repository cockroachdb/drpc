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
}

// AppendFrame adds a frame to the in-progress packet. It returns the completed
// packet and true when a frame with Done=true is received. It returns false
// when more frames are needed to complete the packet.
func (pa *PacketAssembler) AppendFrame(fr Frame) (packet Packet, packetReady bool, err error) {
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
		// New message: reset the buffer and start assembling.
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
