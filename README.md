# BHTTP/1

A small binary protocol for fetching files over TCP, built from scratch: a 12-byte frame header, requests and responses as frames, file bodies as raw bytes, everything on one persistent connection. It is HTTP-shaped but not HTTP, and it is written so that someone who has only read the specification can implement a compatible client or server.

This repository contains the specification, a server (`bserve`), a command-line client (`bcurl`), a proxy that stress-tests other people's clients (`bchaos`), a set of conformance files with checksums, and two independent Python implementations that were written from the specification alone to prove it is enough.

**Building a client and want to test it against this server?** Start with [`docs/INTEROP_GUIDE.md`](docs/INTEROP_GUIDE.md).

## Try it

```bash
go build -o bin/ ./cmd/bserve ./cmd/bcurl ./cmd/bchaos

bin/bserve conformance/www 9000                 # serve the conformance files
bin/bcurl localhost:9000/index.html             # fetch one, body on stdout
bin/bcurl -v localhost:9000/pixel.png > p.png   # -v hexdumps every frame to stderr
```

No Go? `docker build -t bhttp . && docker run --rm -p 9000:9000 bhttp` gives the same server in a 10 MB image.

`bcurl` writes the body to stdout untouched. Exit status is 0 on success, 1 for a 4xx or 5xx answer, 2 for bad usage and 3 if the exchange itself failed. With `-v` every frame that crosses the connection is hexdumped to stderr in full, header and every payload byte, split at frame boundaries, straight from the bytes read and written (nothing is re-encoded for the display). For a large file that is a lot of output, so redirect it: `bcurl -v host:port/big.bin > big.bin 2> wire.txt`. `-H "name: value"` adds a request header.

## Where things are

```
protocol/            the specification
  bhttp1-spec.pdf      two pages: enough to implement it (source: bhttp1-spec.html)
  SPEC.md              full normative behaviour
  WIRE_FORMAT.md       every byte on the wire, and the table of what each fault causes
  HEADER_TABLE.md      the static header table
docs/
  ARCHITECTURE.md      how the code is built and why
  INTEROP_GUIDE.md     testing your client (or server) against this one
cmd/                 bserve, bcurl, bchaos
internal/            frame, protocol, server, client, wire, chaos
conformance/         the files bserve serves for testing, with SHA-256 checksums
tests/interop/       the independent Python client and server, and logs of the runs
examples/hexdump/    a captured request and response, annotated byte by byte
Dockerfile           a ready-to-test server image
```

## Tests

```
go test ./...
go test ./internal/frame -fuzz FuzzRead
```

The suite covers framing under every read boundary (whole, byte at a time, random chunks), short and stalled writes, oversized and truncated frames, malformed requests, the path rules including Windows-specific forms and junction escapes, binary and boundary-sized files (0, 1, 16383, 16384, 16385 bytes and up to 1 MiB), persistent and pipelined connections, many simultaneous clients, idle timeouts, the bounded drain after an error, a `500`, and a file that shrinks mid-transfer. The command-line tests run the real executables and check exit codes and raw stdout bytes, and one of them puts a recording proxy between `bcurl -v` and the server and checks that the bytes rebuilt from the verbose text are exactly the bytes the proxy recorded, in both directions. The conformance folder is checked too: every file's checksum and its frame count are verified against what the server actually sends.

## Interoperability

Two implementations were written by an implementer who saw only the three files in `protocol/`, sharing no code with the Go side:

- `tests/interop/python_client` against `bserve`: 135 checks pass, including a SHA-256 comparison of a binary file (log in `tests/interop/logs`).
- `bcurl` against `tests/interop/python_server`: statuses, exit codes and a byte-identical binary body, plus eight requests on one connection (also logged).

Building them surfaced about seventy places where the first draft of the specification was ambiguous or silent. Each was resolved in the specification rather than in code, and the runs above are against the final text.

`examples/hexdump/annotated-hexdump.md` is one real exchange (a client fetching a 69-byte PNG with one custom request header), captured by a recording proxy sitting between the two programs. The annotation is generated from the captured bytes and checked against the client's own `-v` output and the served file.

`bchaos` is the part meant for other people's clients. It relays a server's answers in random pieces (down to one byte at a time) and mixes in frames of unknown types; a client with sound framing does not notice, and one that assumes a read is a frame, or that rejects unknown types, fails immediately. Its own tests check that it catches exactly those two mistakes.

## Design decisions worth knowing

The reasoning is in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md); in short:

- **Metadata and body are separate frames,** so memory stays bounded whatever the file size and body bytes are never interpreted.
- **Two kinds of failure, two channels.** A well-framed but invalid request gets a `400` and the connection carries on. Anything that makes the stream untrustworthy gets an `ERROR` frame on stream 0 and the connection closes. Unknown frame types are skipped, checked right after the length so nothing else about them can cause a fault.
- **The length is validated before anything is allocated or read,** including for frame types the receiver does not know.
- **After an `ERROR` the sender half-closes and drains for at most one second and 64 KiB,** so a reset cannot destroy the error, and a hostile peer cannot hold the connection open.
- **Empty bodies end on the RESPONSE frame, never on a trailing empty DATA frame,** and a file shorter than announced ends in an `ERROR`, never a clean end of stream.
- **Path rules are the same on every platform,** and the resolved path, including symlinks and junctions, is checked to stay inside the root as a second line of defence.

## Known limits

- The Go race detector needs a C compiler, which the Windows development machine lacks, so it was run in a Linux container instead (`golang:1.23`, Go 1.23.12): the whole suite passes under `-race` with no data races reported. That run is Linux-only, so the Windows-specific code paths (the exclusive-lock 500 test and junction handling) were exercised natively but not under the race detector, and the 500 test is skipped there because the container runs as root.
- The last round of specification clarifications (path and UTF-8 details, links inside the root, reset versus EOF after an error) was not re-read by a fresh blind implementer. The interoperability runs were made against the final text and pass, but that is a weaker check than a new independent reading.
- Interoperability has been shown against two implementations written from the specification, not yet against another person's server.
- There is no version field in the frame header. A different version can only be detected by a fault.
- Requests are sequential in v1. There is no multiplexing, request body, compression or TLS.
