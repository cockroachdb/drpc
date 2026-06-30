// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

// WindowUpdateFrame builds a flow-control credit grant for the given stream id
// and byte delta. Callers must pass a real stream id (streamID > 0; stream id 0
// is reserved for a possible future connection-level window and is unused in
// v1) and a positive delta. These are exactly the conditions ParseWindowUpdate
// enforces: a frame built with streamID == 0 or delta == 0 is well-formed but
// will be rejected on parse, so this helper does not validate them itself (its
// callers, the grant-emit path, are controlled and always satisfy them). The
// frame is marked Control (out-of-band signaling, emitted without blocking on
// data backpressure) and Done (a single self-contained frame).
func WindowUpdateFrame(streamID, delta uint64) Frame {
	return Frame{
		Data:    AppendVarint(nil, delta),
		ID:      ID{Stream: streamID},
		Kind:    KindWindowUpdate,
		Done:    true,
		Control: true,
	}
}

// ParseWindowUpdate extracts the stream id and credit delta from a window update
// frame, enforcing the whole v1 wire contract in one place: the frame must be a
// self-contained control frame (Control and Done set), name a real stream
// (id != 0; stream 0 is reserved for a future connection-level window), and
// carry a positive delta with no trailing bytes. ok is false for any frame that
// does not conform (a non-KindWindowUpdate frame or a malformed/empty payload
// included). A non-conforming frame is meant to be dropped by the caller, not
// acted on or treated as fatal; this also means a reserved stream-0 update from
// a future peer is ignored rather than mishandled. The message id is
// deliberately unconstrained: window updates are intercepted before packet
// assembly, so message-id monotonicity does not apply to them.
func ParseWindowUpdate(fr Frame) (streamID, delta uint64, ok bool) {
	if fr.Kind != KindWindowUpdate || !fr.Control || !fr.Done || fr.ID.Stream == 0 {
		return 0, 0, false
	}
	rem, d, parsed, err := ReadVarint(fr.Data)
	if !parsed || err != nil || len(rem) != 0 || d == 0 {
		return 0, 0, false
	}
	return fr.ID.Stream, d, true
}
