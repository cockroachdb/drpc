# Wire Protocol: Frame Handling

Frames on the wire look like `[stream S, msg M, kind K, done D]`. A packet is
one or more frames that share the same stream and message ID. Each frame carries
a chunk of data; as frames arrive, their data is appended together to assemble
the packet. The packet is complete when the final frame has `done=true`.

Validation happens at two layers: **stream constraints** are per-stream and
shared between mux and non-mux, while **connection constraints** are at the
manager level and differ between mux and non-mux.

---

## Stream Constraints

These are enforced by the stream itself and apply the same way for both mux and
non-mux managers. The stream keeps track of:

- `nextMessageID`: the minimum accepted message ID, bumped to `messageID + 1`
  when a packet completes.
- `assembling`: whether frames are currently being accumulated into a packet.
  Set when the first frame of a packet arrives, cleared when `done=true`
  completes it.
- `pktKind`: the kind of the packet being assembled, set by the first frame.

1. **Stream ID must match.** A misrouted frame is a protocol error.
2. **Message ID must not go backwards.** If the incoming frame's message ID
   (`messageID`) is less than `nextMessageID`, that's a monotonicity violation.
3. **New packet vs continuation.** A frame starts a new packet if:
    - `messageID > nextMessageID` (a higher message arrived; any in-progress
      packet data is discarded), or
    - `messageID == nextMessageID` and `assembling` is false (previous packet
      completed, or this is the first frame on the stream).

    A frame is a continuation if `messageID == nextMessageID` and `assembling`
    is true.

    Discarding in-progress data when a higher message ID arrives supports
    asynchronous interrupts, where the sender abandons a packet mid-write (e.g.,
    due to context cancellation) and moves to a new message. Message IDs do not
    have to be contiguous; a jump from message 1 to message 5 is valid.

4. **Kind must stay consistent within a packet.** Continuation frames (same
   message ID while assembling) must carry the same kind as the first frame of
   that packet.
5. **Append frame data** to the in-progress packet buffer.
6. **Done completes the packet.** `nextMessageID` is bumped to `messageID + 1`
   and the assembling flag is cleared. Any future frame with the same or lower
   message ID gets rejected by rule 2.

## Non-mux Connection Constraints

These live in the non-mux manager's reader loop. A future mux manager will have
its own set of rules since interleaved streams are expected there.

1. **Global frame monotonicity.** Frame IDs on the wire must be non-decreasing.
   The manager tracks the last frame ID it saw, and any frame with a lesser ID
   is a protocol error.
2. **Old-stream frames are silently ignored.** When an incoming frame's stream
   ID is less than the locally created current stream's ID, it gets dropped
   without error. This comes up on the client side where the local stream ID can
   advance before the remote finishes responding. See the examples for how this
   differs from rule 1.
3. **First frame of a stream must be Invoke.** The first frame for a new stream
   has to be `KindInvokeMetadata` or `KindInvoke`. Anything else is a protocol
   error.

## Layering

Both the manager and the stream enforce ordering, but at different scopes.

The stream works at the per-stream level and is the authority on message-level
correctness.

The manager (non-mux) works at the connection level. It catches cross-stream
ordering violations and malformed stream starts. For invoke frames, which the
manager consumes directly and never forwards to a stream, the manager's check is
the only line of defense. Similar to the stream, manager also bumps the ID when
a packet completes. This protection could be left for the streams as it also
does the same, but it's needed at manager level too to protect against the
replay of an Invoke message if a stream is not created by the time next frame
arrives.

---

## Examples

Frames are shown as `[stream S, msg M, kind K, done]` where done is `d=t` (true)
or `d=f` (false). Only relevant fields are included.

### Stream rule 2: message ID must not go backwards

```
[s1, m3, Message, d=t]       <- OK, packet complete, nextMessageID becomes 4
[s1, m2, Message, d=t]       <- error: m2 < nextMessageID(4)
```

### Stream rule 3: new packet vs continuation

```
[s1, m1, Message, d=f]       <- start accumulating
[s1, m1, Message, d=f]       <- continuation, append
[s1, m2, Message, d=f]       <- m1 data silently discarded, m2 starts fresh
```

### Stream rule 4: kind consistency within a packet

```
[s1, m1, Message, d=f]       <- start packet, pktKind=Message
[s1, m1, Error,   d=t]       <- error: kind changed mid-packet
```

Kind is only checked for continuation frames. Different messages can have
different kinds without issue:

```
[s1, m1, Message, d=t]       <- OK, packet complete
[s1, m2, Close,   d=t]       <- OK, new message, no kind check against m1
```

### Stream rule 6: done prevents replay

```
[s1, m1, Message, d=t]       <- packet complete, nextMessageID becomes 2
[s1, m1, Message, d=t]       <- error: m1 < nextMessageID(2)
```

### Stream rule 6: multi-frame then next message

```
[s1, m1, Message, d=f]       <- assembling=true, accumulate
[s1, m1, Message, d=f]       <- continuation, append (kind matches)
[s1, m1, Message, d=t]       <- packet complete, assembling=false, nextMessageID=2
[s1, m2, Close,   d=t]       <- not assembling, new packet, no kind check, OK
```

### Connection rule 1: global monotonicity

```
[s1, m5, d=t]
[s1, m4, d=t]                <- error: {1,4} < lastFrameID {1,5}
```

Cross-stream:

```
[s1, m3, d=t]
[s2, m1, d=t]                <- OK, new stream
[s1, m4, d=t]                <- error: {1,4} < lastFrameID {2,1}
```

Stream 2's frame implicitly starts a new stream. A frame for stream 1 showing up
after that means the sender is writing to a stream it should consider closed.

### Connection rule 2: old-stream frames silently ignored (client side)

This handles a case that rule 1 does not catch. On the client side, the local
stream ID advances independently of the wire:

```
Client creates stream 1, sends invoke.
Server responds:
  [s1, m1, Message, d=t]     <- delivered to stream 1

Client finishes stream 1, creates stream 2 (curr.ID=2).
Server still responding:
  [s1, m2, Close, d=t]       <- s1 < curr(2), silently ignored
```

Global monotonicity passes here (m2 > m1, both on stream 1). But the client has
already moved on. No error is raised because the server doesn't know yet.

### Connection rule 3: first frame must be Invoke

```
[s1, m1, InvokeMetadata, d=t]   <- OK, forwarded
[s1, m2, Invoke, d=t]           <- OK, stream created
[s1, m3, Message, d=t]          <- OK, delivered to stream 1
```

```
[s1, m4, Message, d=t]          <- OK, delivered to stream 1
[s2, m1, Cancel, d=t]          <- error: first frame is not Invoke
```
