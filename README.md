# BHTTP/1: HTTP, in Binary

A small binary, HTTP-like application protocol over persistent TCP.

BHTTP/1 defines its own framing, request and response encoding, headers, error handling and binary-body streaming. It is deliberately not HTTP/1.1 or HTTP/2.

The reference implementation is a Go file server (`bserve`), accompanied by a client (`bcurl`), a transport chaos proxy (`bchaos`), an independent Python client and server, conformance fixtures, and a real, annotated wire capture.

**The protocol is the project.** An independent implementation should be able to communicate with `bserve` using only the specification.

## Why build a protocol from scratch?

The interesting part of BHTTP/1 is not serving a file. It is defining a wire protocol precisely enough that two independently written programs can talk to each other without sharing any code.

That means dealing with TCP read boundaries, binary serialization, frame limits, unknown extensions, malformed input, persistent connections, filesystem security, arbitrary binary bodies, deterministic error behaviour, and interoperability across languages. The Go implementation is one implementation of the protocol, not the protocol itself.

## Protocol at a glance

```text
TCP connection
     │
     ├── REQUEST ────────────────►
     │
     │  ◄──────────── RESPONSE
     │  ◄──────────── DATA
     │  ◄──────────── DATA + END_STREAM
     │
     ├── REQUEST ────────────────►
     │
     │  ◄──────────── RESPONSE + END_STREAM     (empty body, or an error)
     │
     └── same connection, next request
```

Every frame starts with a fixed 12-byte header:

```text
Length u32 | Type u8 | Flags u8 | Reserved u16 | Stream ID u32
```

- Big-endian; maximum payload 16 KiB (16384 bytes)
- Types: REQUEST, RESPONSE, DATA, ERROR. Unknown frame types are skipped by length
- Bodies are opaque bytes and are never interpreted
- Connections are persistent; v1 requests are sequential, with no multiplexing

The two-page specification is [`protocol/bhttp1-spec.pdf`](protocol/bhttp1-spec.pdf).

## Quick start

Build:

```bash
go build -o bin/ ./cmd/bserve ./cmd/bcurl ./cmd/bchaos
```

Start the server on the conformance files:

```bash
bin/bserve conformance/www 9000
```

Fetch a file. The body goes to stdout, untouched:

```bash
bin/bcurl localhost:9000/index.html
```

Inspect the wire. Every frame in both directions is hexdumped to stderr, in full, straight from the bytes read and written (nothing is re-encoded for the display):

```bash
bin/bcurl -v localhost:9000/index.html > index.html 2> wire.txt
```

`-H "name: value"` adds a request header. `bcurl` exits 0 on success, 1 for a 4xx or 5xx answer, 2 for bad usage and 3 if the exchange itself failed.

No Go installed? Use Docker (the image is about 10 MB):

```bash
docker build -t bhttp .
docker run --rm -p 9000:9000 bhttp
```

## Independent interoperability

Two implementations were written by an implementer who saw only the three files in `protocol/`, sharing no code with the Go side:

- `tests/interop/python_client` against `bserve`: 135 checks pass, including a SHA-256 comparison of a binary file (log in `tests/interop/logs`).
- `bcurl` against `tests/interop/python_server`: statuses, exit codes and a byte-identical binary body, plus eight requests on one connection (also logged).

Building them surfaced about seventy places where the first draft of the specification was ambiguous or silent. Each was resolved in the specification rather than in code, and the runs above are against the final text.

`examples/hexdump/annotated-hexdump.md` is one real exchange (a client fetching a 69-byte PNG with one custom request header), captured by a recording proxy sitting between the two programs. The annotation is generated from the captured bytes and checked against the client's own `-v` output and the served file.

## Conformance and chaos testing

Building a client and want to test it against this server? Start with [`docs/INTEROP_GUIDE.md`](docs/INTEROP_GUIDE.md). It has a checklist with expected results, known-good byte captures, and how to triage a mismatch.

- **`conformance/`** holds eleven files with SHA-256 checksums, chosen so every case a client can get wrong has a file that exposes it: the empty file, one byte under a full frame, exactly a full frame, one over, four frames, all 256 byte values, and a UTF-8 path.
- **`bchaos`** is a proxy that relays a server's answers in random pieces (down to one byte at a time) and mixes in frames of unknown types. A client with sound framing does not notice; one that assumes a read is a frame, or that rejects unknown types, fails immediately. Its own tests check that it catches exactly those two mistakes.

## Architecture

```text
                 cmd/  bserve · bcurl · bchaos
                         │
        ┌────────────────┼────────────────┐
   internal/server  internal/client  internal/chaos
   connection loop   response rules    transport stress
   path resolver           │
        │                  │
        └────────┬─────────┘
          internal/protocol      request, response, header, error codecs
                 │
          internal/frame         12-byte header, exact reads, complete writes
                 │
                TCP
```

`internal/wire` is a passive tap that prints what crossed a connection, and it is what `bcurl -v` uses. The server resolves each path against the document root and then opens the file; see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the reasoning behind each layer.

## Project structure

```text
protocol/           the specification: two-page PDF, full text, wire format, header table
cmd/                bserve, bcurl, bchaos
internal/frame/     binary framing
internal/protocol/  message and header codecs
internal/server/    connection handling, path resolution, file serving
internal/client/    the client and its response rules
internal/wire/      the tap behind bcurl -v
internal/chaos/     transport and framing stress tool
conformance/        deterministic fixtures with checksums
tests/interop/      independent Python client and server, and logs of the runs
examples/hexdump/   a real captured exchange, annotated byte by byte
docs/               architecture, and the interoperability guide
Dockerfile          a ready-to-test server image
```

## Notable design decisions

The reasoning is in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md); in short:

- **Metadata and body are separate frames,** so memory stays bounded whatever the file size and body bytes are never interpreted.
- **Two kinds of failure, two channels.** A well-framed but invalid request gets a `400` and the connection carries on. Anything that makes the stream untrustworthy gets an `ERROR` frame on stream 0 and the connection closes.
- **Unknown frame types are skipped, and the length is validated before anything is allocated,** including for types the receiver does not know. The unknown-type check comes right after the length check, so nothing else about such a frame can cause a fault.
- **After an `ERROR` the sender half-closes and drains for at most one second and 64 KiB,** so a reset cannot destroy the error and a hostile peer cannot hold the connection open.
- **A truncated download can never look complete.** Empty bodies end on the RESPONSE frame, never on a trailing empty DATA frame, and a file that turns out shorter than announced ends in an `ERROR`, never a clean end of stream.
- **Path rules are the same on every platform,** and the resolved path, including symlinks and junctions, is checked to stay inside the root as a second line of defence.

## Tests

```bash
go vet ./...
go test ./...
go test ./internal/frame -fuzz FuzzRead
```

| Area | Coverage |
|---|---|
| Framing | partial reads, one-byte reads, random chunks, short and stalled writes, oversized and truncated frames |
| Fuzzing | 4.7 million frame-reader inputs, plus every payload decoder |
| Filesystem | traversal, symlinks, junctions, Windows-specific path forms and device names |
| Binary data | arbitrary bytes; boundary sizes 0, 1, 16383, 16384, 16385 bytes and up to 1 MiB |
| Connections | persistent, pipelined, many simultaneous clients, idle timeouts, the bounded drain after an error |
| Faults | `400`, `404`, `500`, `ERROR`, a file that shrinks mid-transfer |
| Command line | the real executables: exit codes and raw binary stdout |
| Wire evidence | a real capture, and a test that rebuilds the `-v` output's bytes and compares them with a recording proxy, both directions |
| Conformance | every fixture file's checksum and frame count checked against what the server sends |
| Race detector | the full suite (368 tests and subtests) under `-race` in a Linux container |

## Documentation

| | |
|---|---|
| [`protocol/bhttp1-spec.pdf`](protocol/bhttp1-spec.pdf) | the two-page specification (source: `bhttp1-spec.html`) |
| [`protocol/SPEC.md`](protocol/SPEC.md) | full normative behaviour |
| [`protocol/WIRE_FORMAT.md`](protocol/WIRE_FORMAT.md) | every byte on the wire, and what each fault causes |
| [`protocol/HEADER_TABLE.md`](protocol/HEADER_TABLE.md) | the static header table |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | how the code is built and why |
| [`docs/INTEROP_GUIDE.md`](docs/INTEROP_GUIDE.md) | testing your client or server against this one |
| [`examples/hexdump/annotated-hexdump.md`](examples/hexdump/annotated-hexdump.md) | a captured exchange, annotated byte by byte |

## Status

BHTTP/1 is complete and frozen. The submitted implementation is intentionally not being changed; any follow-up work is developed on separate branches.

## Verification limits

- No interoperability run has yet been performed against an unrelated third-party implementation. Two implementations written from the specification alone stand in for one.
- The two-page specification is derived by hand from the full specification and reviewed; it is not generated from it or mechanically diff-checked. A script did confirm that every number and fact in it also appears in the full text.
- The race suite runs on Linux (`golang:1.23`, Go 1.23.12) because the Windows development machine has no C compiler. Windows-specific behaviour (the exclusive-lock `500` test, junction handling) is tested natively on Windows but not under the race detector, and the `500` test is skipped in the Linux run because the container runs as root.
- The latest wording clarifications to the specification (path and UTF-8 details, links inside the root, reset versus EOF after an error) were checked by re-running the interoperability suites, not by a new independent reading.
- The v1 frame header has no version field. A different version can only be detected by a fault.
- v1 is intentionally sequential: no multiplexing, request bodies, compression or TLS.
