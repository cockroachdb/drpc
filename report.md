# DRPC Mux Performance Analysis

**Date:** 2026-04-21 **Machine:** 24-core Intel Xeon @ 2.80GHz, Linux **Branch:** `shubham/enable-stream-multiplexing`

## Benchmark Results

### Bidi N=64

| Config | Code path | TCP conns | ns/op | B/op | allocs/op | CPU utilization |
| --- | --- | --- | --- | --- | --- | --- |
| Non-mux connperstream | `Writer` + `packetBuffer` | 64 | 1790 | 240 | 3 | 2109% (21 cores) |
| Mux connperstream | `MuxWriter` + `ringBuffer` | 64 | 1845 | 278 | 3 | 2148% (21 cores) |
| Mux shared | `MuxWriter` + `ringBuffer` | 1 | 2674 | 250 | 3 | 505% (5 cores) |

### Unary N=1

| Config                | ns/op | B/op | allocs/op |
| --------------------- | ----- | ---- | --------- |
| Non-mux connperstream | 54685 | 1547 | 7         |
| Mux shared            | 55885 | 6746 | 19        |

## Key Findings

### 1. The mux machinery itself is cheap (3% overhead)

Comparing non-mux connperstream (1790 ns/op) vs mux connperstream (1845 ns/op) isolates the code path overhead: `MuxWriter` + `ringBuffer` + per-stream `PacketAssembler` vs the old `Writer` + `packetBuffer` + shared `ReadPacketUsing`. The result is only 3% slower, with nearly identical CPU profiles (both saturate ~21 cores, both spend ~62% in syscalls).

The `MuxWriter` lock-cond-swap-write pattern and the `ringBuffer` lock-copy-broadcast pattern work well when each connection carries one stream.

### 2. The 36% bidi penalty is I/O serialization

The gap from 1845 to 2674 ns/op comes from funneling 64 streams through one TCP connection. CPU utilization drops from 2148% to 505% — the work that was spread across 21 cores now bottlenecks on two serial goroutines:

- **`MuxWriter.run`** (8.81s cum, 26.8%): one goroutine drains the shared write buffer into a single TCP `Write` syscall. 64 stream goroutines contend on `mw.mu` to append frames.

- **`manageReader`** (7.33s cum, 22.3%): one goroutine reads all frames from the TCP fd and dispatches them to per-stream ring buffers.

Together they account for 49% of all CPU time in the mux shared config. The kernel can parallelize reads/writes across 64 independent fds, but not across one.

### 3. Contention overhead is measurable

Flat CPU time spent purely on synchronization in mux shared:

| Function                | Flat time         |
| ----------------------- | ----------------- |
| `runtime.futex`         | 1.45s (4.4%)      |
| `atomic.CompareAndSwap` | 1.17s (3.6%)      |
| `sync.Mutex.Lock`       | 1.16s (3.5%)      |
| `sync.Mutex.Unlock`     | 0.69s (2.1%)      |
| **Total contention**    | **4.47s (13.6%)** |

Go scheduler overhead is also elevated:

| Function               | Flat time     |
| ---------------------- | ------------- |
| `runtime.schedule`     | 6.49s (19.7%) |
| `runtime.findRunnable` | 4.52s (13.7%) |
| `runtime.stealWork`    | 2.09s (6.4%)  |

This scheduler churn is driven by `Cond.Broadcast` in the ring buffer — each enqueue/dequeue wakes goroutines, which must be scheduled onto cores. The block profile confirms: mux connperstream shows 1367s of `Cond.Wait` blocking vs 661s for non-mux (75% more), even though wall-clock performance is similar. Under single-connection contention, this churn compounds.

### 4. Ring buffer `Broadcast` is overkill

The `ringBuffer` is single-producer (manageReader) / single-consumer (RPC handler goroutine). Both `Enqueue` and `Dequeue` call `rb.cond.Broadcast()`, which wakes _all_ waiters. For a single-producer/single-consumer queue, only one waiter can ever be blocked — `Signal()` would suffice and avoids waking goroutines unnecessarily.

### 5. Unary allocations: 12 extra per RPC

The mux path allocates 19 objects per unary RPC vs 7 for connperstream. The extras come from:

| Source | Allocs | Bytes | Why |
| --- | --- | --- | --- |
| `ringBuffer.init` — `make([]*[]byte, 256)` | 2 (client+server) | 4096 | Decouples reader from consumer; old `packetBuffer` was zero-alloc |
| `PacketAssembler.AppendFrame` — `append` growth | 3-4 | ~400 | Each stream owns a PA; old path reused one PA per connection via `ReadPacketUsing` |
| `handleInvokeFrame` — `&pendingStream{}` | 1 | ~80 | New struct to assemble invoke/metadata frames |
| `NewServerStream` — `string(pkt.data)` | 1 | ~40 | Byte-to-string copy of RPC name |
| `Signal.signalSlow` — `make(chan struct{})` | 2 | 192 | `Finished()` channel created lazily; old path used pre-allocated `sfin` |
| `newStream` — goroutine stack | 1-2 | ~2048 | `go manageStream()`; old path had a single `manageStreams` goroutine |
| `BufferPool` — sync.Pool miss | 0-1 | 4096 | Pool cold start |

## Suggestions

### S1. Replace `Broadcast` with `Signal` in ringBuffer

`ringBuffer` is strictly single-producer/single-consumer. Replace `rb.cond.Broadcast()` with `rb.cond.Signal()` in both `Enqueue` and `Dequeue`. This eliminates unnecessary goroutine wake-ups that contribute to the 75% extra blocking time measured in the block profile.

**Impact:** Reduces scheduler churn. Low risk — the invariant (one producer, one consumer) is structural.

**Files:** `drpcstream/ring_buffer.go:67,89`

### S2. Start ringBuffer at small capacity, grow on demand

`make([]*[]byte, 256)` allocates 2048 bytes per stream. Most streams (especially unary RPCs) will never buffer more than 1-2 messages. Start at capacity 4 and double when full, or use a fixed small size (e.g., 8).

**Impact:** Cuts 4096 bytes/RPC from unary allocations. For bidi, the buffer will grow once and stay grown.

**Files:** `drpcstream/ring_buffer.go:14,44`

### S3. Pool `pendingStream` in handleInvokeFrame

The TODO on `manager.go:254` already notes this. Keep a `sync.Pool` of `pendingStream` values and reset them between uses. Each unary RPC currently allocates a new one.

**Impact:** Eliminates 1 alloc + ~80 bytes per server-side RPC.

**Files:** `drpcmanager/manager.go:225,254`

### S4. Pre-allocate the `Finished()` signal channel

Currently `drpcsignal.Signal` lazily allocates a `chan struct{}` on first `Signal()` call. Since every stream's `manageStream` goroutine calls `<-stream.Finished()`, this always triggers the slow path. Pre-create the channel during stream construction.

**Impact:** Eliminates 2 allocs (client+server) + 192 bytes per RPC.

**Files:** `drpcsignal/signal.go:42-49`, `drpcstream/stream.go:90-119`

### S5. Pre-allocate backing array in PacketAssembler

`NewPacketAssembler` starts with a nil `pk.Data`. The first `AppendFrame` call triggers `append` growth. Pre-allocate a small backing array (e.g., 128 bytes) to avoid the growth allocation for typical small messages.

**Impact:** Eliminates 3-4 allocs per RPC across client PA, server PA, and pendingStream PA.

**Files:** `drpcwire/packet_assembler.go:22-28`

### S6. Avoid `string(pkt.data)` copy in NewServerStream

Use `unsafe.String` (Go 1.20+) to create a string from the byte slice without copying, if the data lifetime permits. The RPC name bytes come from `PacketAssembler.pk.Data` which gets overwritten on the next packet — so the string must be copied _or_ the consumer must not hold a reference past the next read. Evaluate whether the RPC name can be interned (there are typically few distinct RPC names).

**Impact:** Eliminates 1 alloc + ~40 bytes per RPC.

**Files:** `drpcmanager/manager.go:358`

### S7. Batch frame dispatch in manageReader

Currently `manageReader` reads one frame, locks the target ring buffer, copies data, broadcasts, unlocks, then reads the next frame. If the TCP read buffer contains multiple frames for the same stream (common under load), batch them into one lock acquisition and one signal.

**Impact:** Reduces lock round-trips and wake-ups under high concurrency. More complex to implement — requires buffering frames or peeking at the read buffer.

**Files:** `drpcmanager/manager.go:181-217`

### S8. Consider per-stream channels instead of ringBuffer cond vars

Replace the `sync.Mutex` + `sync.Cond` in `ringBuffer` with a buffered channel of `*[]byte`. Channels use targeted goroutine wake-ups (no broadcast storm) and the Go runtime's integrated scheduler support for channel operations. The trade-off is losing the ability to do a bounded ring with back-pressure — but a buffered channel of capacity 256 achieves the same thing.

**Impact:** Eliminates the `Cond.Broadcast` wake-up storm entirely. May improve bidi throughput measurably. Needs benchmarking — channel overhead vs cond var overhead is workload-dependent.

**Files:** `drpcstream/ring_buffer.go` (rewrite)

## Profile Locations

| Profile                      | Path                                |
| ---------------------------- | ----------------------------------- |
| Mux shared CPU               | `/tmp/cpu_bidi_mux.out`             |
| Mux shared memory            | `/tmp/mem_unary_mux.out`            |
| Mux connperstream CPU        | `/tmp/connperstream_bidi_cpu.out`   |
| Mux connperstream memory     | `/tmp/connperstream_bidi_mem.out`   |
| Mux connperstream block      | `/tmp/connperstream_bidi_block.out` |
| Mux connperstream mutex      | `/tmp/connperstream_bidi_mutex.out` |
| Non-mux connperstream CPU    | `/tmp/nonmux_bidi_cpu.out`          |
| Non-mux connperstream memory | `/tmp/nonmux_bidi_mem.out`          |
| Non-mux connperstream block  | `/tmp/nonmux_bidi_block.out`        |
