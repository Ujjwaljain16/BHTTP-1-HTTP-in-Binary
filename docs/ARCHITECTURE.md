# Architecture

BHTTP/1 is a small binary protocol for fetching files over one persistent TCP connection. This document explains how the implementation is put together and, more importantly, why it is put together that way. The protocol itself is specified in `protocol/`; this is about the code.

## The shape of it

Bytes travel up through four layers and every layer knows only about the one below it.

```
   cmd/bserve   cmd/bcurl   cmd/bchaos          thin programs: arguments, exit codes, output
        │           │            │
   internal/server  internal/client  internal/chaos       behaviour: what requests mean, how answers are checked
        │           │            │
        └───────────┴─────┬──────┘
                          │
                  internal/protocol                 messages: request, response, headers, error payloads
                          │
                   internal/frame                   framing: the 12-byte header, exact reads, complete writes
                          │
                        TCP
                                        internal/wire: an observer that prints what crossed the connection
```

The rule that keeps this honest is the dependency direction: `frame` and `protocol` import nothing of ours, the behaviour packages build on those two, and nothing below `cmd/` knows that a command line exists. There is no `net/http` anywhere; every byte on the wire is produced and consumed by code in this repository.

| Package | Responsibility | Deliberately does not |
|---|---|---|
| `internal/frame` | Read and write frames. Enforce the 16 KiB limit before allocating. Skip frame types it does not know. Reject stream ID 0 on request, response and data frames. | Know what a request is, what a status means, or that files exist. |
| `internal/protocol` | Encode and decode request, response, header and error payloads with strict exact-consumption rules. | Decide whether a path is acceptable. It checks length and UTF-8; the server decides the rest. |
| `internal/server` | The connection loop, path resolution, file streaming, the error and drain behaviour. | Parse bytes itself. It asks `frame` and `protocol`. |
| `internal/client` | Send a request, then hold the answer to the response rules. Never reconnects. | Print anything or exit the process. |
| `internal/wire` | Wrap a connection and dump every frame, in both directions, from the raw bytes. | Change a byte. It is a passive tap. |
| `internal/chaos` | Relay a server's answers to a client in awkward pieces and with unknown frames mixed in. | Touch requests, or know what any frame means beyond its length. |

## How a frame is read

This is the core invariant of the whole project: at every moment the reader knows exactly where the current frame starts, where its payload ends and where the next one begins. TCP delivers arbitrary pieces, so nothing may depend on how the bytes arrive.

```
read exactly 12 bytes ── EOF at zero bytes ──► clean end
        │                 EOF partway ───────► truncated: discard, close, say nothing
        ▼
Length > 16384?  ── yes ──► ERROR(1) then close        (checked before any allocation, for every type)
        │
        ▼
type unknown?    ── yes ──► read and discard Length bytes, carry on   (flags, reserved, stream ID never examined)
        │
        ▼
read exactly Length bytes ──► stream ID 0 on types 1 to 3? ─► ERROR(2) then close
        │
        ▼
hand the frame to whoever owns its meaning
```

The order is not incidental. The length check comes first and applies even to unknown types, because a hostile length is dangerous whatever the type is. The unknown-type check comes before everything else about the frame, because if an unknown frame with stream ID 0 or strange flags could cause a fault, then adding a frame type in a future version would break every old peer, which defeats the point of being able to skip.

The reader lives in `internal/frame/reader.go` and is about sixty lines. It is tested against a plain buffer, a reader returning one byte at a time, a reader returning half of what is asked, and a reader returning random small pieces, and it is fuzzed: whatever the input, it must not panic, must not allocate more than a frame, and must produce the same frames however the bytes are sliced.

## The three kinds of failure

Deciding what to do when something is wrong is where protocols get sloppy, so every fault has exactly one outcome, and the outcome depends on whether the byte stream can still be trusted.

| Situation | Can the stream be trusted? | Outcome |
|---|---|---|
| A well-framed request whose contents are invalid (bad method, bad path, trailing bytes, missing END_STREAM, a RESPONSE sent to a server) | Yes: the frame boundary is known | `RESPONSE 400` on that stream, connection continues |
| A valid request for something that is not there, or is outside the root | Yes | `RESPONSE 404`, connection continues |
| The server fails before it has sent anything | Yes | `RESPONSE 500`, connection continues |
| The server fails after the RESPONSE is out (a file that turns out shorter than announced) | No: the client already believes a body is coming | `ERROR` frame, then close. Never a clean end of stream, so a truncated download cannot look complete. |
| A length above 16384, or stream ID 0 | No: we cannot say where the next frame starts, or which stream to blame | `ERROR` on stream 0, then close |
| A frame of an unknown type | Yes | Skipped |
| Input ends in the middle of a frame | There is nobody left to tell | Discard, close, send nothing |

Two consequences are worth stating. First, `400` and `ERROR` are different channels on purpose: an invalid request does not poison the connection, but an untrustworthy frame boundary does. Second, an `ERROR` is never answered, by anyone, ever, so two peers cannot bounce errors at each other.

### Closing after an error without losing it

If a program closes a TCP connection while unread data is waiting for it, the operating system may send a reset, and a reset can arrive before the peer has read the last thing we sent, destroying the `ERROR` we just wrote. So after sending an `ERROR` the server half-closes (sends a FIN), then reads and discards whatever the peer is still sending, for at most one second and 64 KiB, then closes. Both limits matter: without them a hostile peer could keep the connection open forever by never stopping.

## Serving a file

```
REQUEST frame ──► flags ok? ──► decode payload ──► path rules ──► resolve ──► open ──► stream ──► next frame
                    │ no           │ malformed        │ violates     │ missing / outside root
                    ▼              ▼                  ▼              ▼
                   400            400                400            404
```

**Path rules and resolution are separate, and both exist.** `checkPath` rejects anything the filesystem could read differently from how we read it: `..` segments, backslashes, colons (alternate data streams and drive letters on Windows), device names such as `CON` and `NUL` with any extension, trailing dots and spaces (which Windows silently drops), control bytes and doubled slashes. These rules are identical on every platform, so a path behaves the same wherever the server runs. Then `resolve` follows symbolic links and junctions and confirms the final location is still inside the document root, because string checks cannot see a link that points outward. A path that resolves outside gets a plain `404`, so a probe learns nothing about what exists elsewhere. Both layers are tested independently, including with a junction created at test time.

**The size comes from the open file.** The server opens the file, asks the open handle for its size, and announces that in `content-length`. It never stats by name and reads later, so the announced length and the bytes sent come from the same file even if the file is replaced in between. If the file then turns out shorter, the outcome is the `ERROR` above rather than a short body.

**Bodies stream.** A file is read in chunks of at most 16384 bytes, each sent as a DATA frame, the last carrying END_STREAM. Memory use does not depend on the size of the file. An empty body ends on the RESPONSE frame itself, and a file whose size is an exact multiple of 16384 ends on its last full frame; there is never a trailing empty DATA frame, so a receiver never has to wonder whether one is coming.

## Concurrency and lifetime

One goroutine per connection, sharing nothing but the immutable resolver. A connection handles its requests strictly in order, so pipelined requests are answered in order without interleaving, and no locking is needed inside a connection. The server tracks its connections so that `Close` can stop accepting, close every open connection and wait for the handlers to finish; this is what makes the tests deterministic.

Every read has a deadline (30 seconds of idleness, which also bounds a peer that sends half a frame and stops) and every write has one, so a peer that stops reading cannot hold a goroutine forever.

## The client and the tap

`internal/client` is the mirror image of the server: it sends a request with END_STREAM, then insists on a RESPONSE for its own stream followed by DATA for the same stream. Anything else (a frame for another stream, data before a response, a second response, an `ERROR`, the connection ending early, a `content-length` that disagrees with the bytes received) is a failure, after which the client closes the connection and refuses further requests rather than reconnecting. That last point is deliberate: reconnecting silently would hide exactly the problems this protocol exists to expose. Stream IDs count up from 1 and are never reused.

`internal/wire` is what `bcurl -v` uses, and its design is the reason the hexdump can be trusted: it wraps the `net.Conn`, so it sees the bytes as they are read and written, and it only ever reads the length field to find where frames end. It cannot print something the connection did not carry, because it has no encoder. A test puts a recording proxy between `bcurl -v` and a server and checks that the bytes rebuilt from the printed text equal the bytes the proxy recorded, in both directions.

## How the design was checked

Testing works at several distances from the code, because each catches different mistakes.

- **Unit and table tests** for every codec and rule, including boundary sizes and malformed input.
- **Fuzzing** of the frame reader and every payload decoder, with round-trip properties: anything a decoder accepts must re-encode and decode to the same thing.
- **Fault injection.** The interesting failure paths (a file that shrinks mid-transfer, a file that cannot be opened, a peer that never stops sending after an error) have tests that create the fault for real, and the tests were confirmed to fail when the behaviour was deliberately broken.
- **The command-line programs** are tested as executables: exit codes and the raw bytes on stdout.
- **Two independent implementations written from the specification alone** (a Python client and a Python server, by an implementer who never saw the Go code). Building them exposed about seventy places where the first draft of the specification was ambiguous or silent; each was resolved in the specification, not patched in code. The final runs against the final text pass in both directions.
- **A real capture,** annotated byte by byte from a recording proxy.
- **`bchaos`,** which stresses a client's framing with one-byte delivery and unknown frames, and is itself tested against deliberately broken clients to prove it catches them.

The race detector needs a C compiler that the Windows development machine lacks, so the full suite was run under `-race` in a Linux container: clean.

## Decisions and their reasons

| Decision | Reason |
|---|---|
| Metadata and body are separate frames | Keeps every frame small and bounded, lets a file of any size stream, and keeps the body opaque bytes the protocol never inspects. |
| A 12-byte header with a 32-bit length | The length only needs 14 bits today. 32 keeps the header 4-byte aligned and lets a later version raise the cap without changing the layout. |
| Unknown frame types are skipped before any other check | It is what makes the protocol extensible: a newer peer can send a frame type an older one has never seen without breaking it. |
| `400` for content faults, `ERROR` plus close for framing faults | Whether the connection survives should depend on whether the byte stream can still be trusted, and it should be the same answer for every implementation. |
| Reserved bits are ignored, but undefined flag bits on a request are rejected | Reserved has no defined content, so ignoring it costs nothing. A server that rejects undefined request flags catches faulty clients deterministically, while a client ignores unknown flags on a response so a newer server can add them. |
| Path rules are the same on every platform | A path should not mean one thing on Linux and another on Windows. |
| Stream IDs exist although requests are sequential | Cheap now, and they make a later move to concurrent requests possible without a new header. |
| No version field | Keeps the header minimal. A different version can only be detected by a fault, which is a known limit, not an oversight. |
| An empty body ends on the RESPONSE frame | One rule with no exceptions is easier to implement correctly than "send an empty frame unless...". |

## Limits, stated plainly

- Requests are sequential: no multiplexing, no request bodies, no compression, no TLS.
- There is no version negotiation.
- Interoperability has been demonstrated against two implementations written from the specification, not against another person's server. `docs/INTEROP_GUIDE.md` is written so that can be done in an afternoon.
- The race-detector run was on Linux, so the Windows-only code paths (the exclusive-lock test for `500` and junction handling) ran natively but not under the detector.
