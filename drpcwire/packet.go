// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import "fmt"

//go:generate stringer -type=Kind -trimprefix=Kind -output=packet_string.go

// Kind is the enumeration of all the different kinds of messages drpc sends.
type Kind uint8

const (
	// kindReserved is saved for the future in case we need to extend.
	//   kindReserved Kind = 0

	// KindInvoke is used to invoke an rpc. The body is the name of the rpc.
	KindInvoke Kind = 1

	// KindMessage is used to send messages. The body is an encoded message.
	KindMessage Kind = 2

	// KindError is used to inform that an error happened. The body is an error
	// with a code attached.
	KindError Kind = 3

	// KindCancel is sent to notify the remote that we have soft canceled.
	KindCancel Kind = 4

	// KindClose is used to inform that the rpc is dead. It has no body.
	KindClose Kind = 5

	// KindCloseSend is used to inform that no more messages will be sent.
	// It has no body.
	KindCloseSend Kind = 6 // body must be empty

	// KindInvokeMetadata includes metadata about the next Invoke packet.
	KindInvokeMetadata Kind = 7

	// KindWindowUpdate carries a flow-control credit grant: a varint byte delta
	// for the frame's stream id. Stream id 0 is reserved for a future
	// connection-level window and is unused in v1.
	KindWindowUpdate Kind = 8
)

//
// packet id
//

// ID represents a packet id.
type ID struct {
	// Stream is the stream identifier.
	Stream uint64

	// Message is the message identifier.
	Message uint64
}

// Less returns true if the id is less than the provided one. An ID is less than
// another if the Stream is less, and if the stream is equal, if the Message
// is less.
func (i ID) Less(j ID) bool {
	return i.Stream < j.Stream || (i.Stream == j.Stream && i.Message < j.Message)
}

// String returns a human readable form of the ID.
func (i ID) String() string { return fmt.Sprintf("<id s:%d m:%d>", i.Stream, i.Message) }

//
// data frame
//

// Frame is a split data frame on the wire.
type Frame struct {
	// Data is the payload of bytes.
	Data []byte

	// ID is used so that the frame can be reconstructed.
	ID ID

	// Kind is the kind of the payload.
	Kind Kind

	// Done is true if this is the last frame for the ID.
	Done bool

	// Control is true if the frame has the control bit set.
	Control bool
}

// String returns a human readable form of the packet.
func (fr Frame) String() string {
	return fmt.Sprintf("<frm s:%d m:%d data:%d kind:%s done:%v>",
		fr.ID.Stream, fr.ID.Message, len(fr.Data), fr.Kind, fr.Done)
}

// ParseFrame attempts to parse a frame at the beginning of buf. If successful
// then rem contains the unparsed data, fr contains the parsed frame, ok will
// be true, and err will be nil. If there is not enough data for a frame, ok
// will be false and err will be nil. If the data in the buf is malformed, then
// an error is returned.
func ParseFrame(buf []byte) (rem []byte, fr Frame, ok bool, err error) {
	var length uint64
	var control byte
	if len(buf) < 4 {
		goto bad
	}

	rem, control = buf[1:], buf[0]
	fr.Done = (control & 0b00000001) > 0
	fr.Control = (control & 0b10000000) > 0
	fr.Kind = Kind((control & 0b01111110) >> 1)
	rem, fr.ID.Stream, ok, err = ReadVarint(rem)
	if !ok || err != nil {
		goto bad
	}
	rem, fr.ID.Message, ok, err = ReadVarint(rem)
	if !ok || err != nil {
		goto bad
	}
	rem, length, ok, err = ReadVarint(rem)
	if !ok || err != nil || length > uint64(len(rem)) {
		goto bad
	}
	rem, fr.Data = rem[length:], rem[:length]

	return rem, fr, true, nil
bad:
	return buf, fr, false, err
}

// AppendFrame appends a marshaled form of the frame to the provided buffer.
func AppendFrame(buf []byte, fr Frame) []byte {
	control := byte(fr.Kind << 1)
	if fr.Done {
		control |= 0b00000001
	}
	if fr.Control {
		control |= 0b10000000
	}

	out := buf
	out = append(out, control)
	out = AppendVarint(out, fr.ID.Stream)
	out = AppendVarint(out, fr.ID.Message)
	out = AppendVarint(out, uint64(len(fr.Data)))
	out = append(out, fr.Data...)
	return out
}

// WindowUpdateFrame builds a KindWindowUpdate grant frame. Callers must pass a
// real stream id (>0; 0 is reserved) and a positive delta, the conditions
// ParseWindowUpdate enforces.
func WindowUpdateFrame(streamID, delta uint64) Frame {
	return Frame{
		Data:    AppendVarint(nil, delta),
		ID:      ID{Stream: streamID},
		Kind:    KindWindowUpdate,
		Done:    true,
		Control: true,
	}
}

// ParseWindowUpdate returns a grant frame's stream id and delta. ok is false
// unless the frame conforms to the wire contract, so the caller drops
// non-conforming frames rather than acting on them.
func ParseWindowUpdate(fr Frame) (streamID, delta uint64, ok bool) {
	// A self-contained control frame for a real stream; the message id is
	// unchecked since grants are intercepted before packet assembly.
	if fr.Kind != KindWindowUpdate || !fr.Control || !fr.Done || fr.ID.Stream == 0 {
		return 0, 0, false
	}
	rem, d, parsed, err := ReadVarint(fr.Data)
	if !parsed || err != nil || len(rem) != 0 || d == 0 { // positive delta, no trailing bytes
		return 0, 0, false
	}
	return fr.ID.Stream, d, true
}

//
// packet
//

// Packet is a single message sent by drpc.
type Packet struct {
	// Data is the payload of the packet.
	Data []byte

	// ID is the identifier for the packet.
	ID ID

	// Kind is the kind of the packet.
	Kind Kind

	// Control is set to true for packets that are
	// forwards compatible. Unknown or invalid packets
	// with the control bool set should be ignored
	// instead of triggering any errors.
	Control bool
}

// String returns a human readable form of the packet.
func (p Packet) String() string {
	return fmt.Sprintf("<pkt s:%d m:%d data:%d kind:%s>",
		p.ID.Stream, p.ID.Message, len(p.Data), p.Kind)
}
