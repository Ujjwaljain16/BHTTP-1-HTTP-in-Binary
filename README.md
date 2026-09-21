# BHTTP/1

A small binary protocol for fetching files over TCP, built from scratch: a 12-byte frame header, requests and responses as frames, file bodies as raw bytes, everything on one persistent connection. It is HTTP-shaped but not HTTP, and it is written to be implemented by someone who has only read the specification.

This repository holds the protocol specification, a server (`bserve`), a command-line client (`bcurl`), and two independent Python implementations that were written from the specification alone to check that it really is enough.

## Layout

```
protocol/            the specification
  bhttp1-spec.pdf      two-page version (source: bhttp1-spec.html)
  SPEC.md              full normative behavior
  WIRE_FORMAT.md       every byte on the wire, and the fault/outcome table
  HEADER_TABLE.md      static header table
cmd/bserve           file server
cmd/bcurl            client with -v hexdump of the real bytes
internal/frame       frame reader and writer
internal/protocol    request, response, header and error codecs
internal/server      connection loop, path resolver, file streaming
internal/client      client that follows the response rules
internal/wire        the tap behind bcurl -v
tests/interop        independent Python client and server, logs of the runs
examples/hexdump     a captured request and response, annotated byte by byte
```

## Build and run

```
go build -o bin/ ./cmd/bserve ./cmd/bcurl

bin/bserve ./www 9000
bin/bcurl localhost:9000/index.html > page.html
bin/bcurl -v -H "x-demo: hello" localhost:9000/pixel.png > pixel.png
```

`bcurl` writes the body to stdout untouched. Exit status is 0 on success, 1 for a 4xx or 5xx answer, 2 for bad usage and 3 if the exchange itself failed. With `-v` every frame that crosses the connection is hexdumped to stderr, split at frame boundaries, straight from the bytes read and written (nothing is re-encoded for the display).

## Tests

```
go test ./...
go test ./internal/frame -fuzz FuzzRead
```

The suite covers framing under every read boundary (whole, byte at a time, random chunks), short and stalled writes, oversized and truncated frames, malformed requests, the path rules including Windows-specific forms and junction escapes, binary and boundary-sized files (0, 1, 16383, 16384, 16385 bytes and up to 1 MiB), persistent and pipelined connections, many simultaneous clients, idle timeouts, the bounded drain after an error, a `500`, and a file that shrinks mid-transfer. The command-line tests run the real executables and check exit codes and raw stdout bytes.

## Interoperability

Two implementations were written by an implementer who saw only the three files in `protocol/`, sharing no code with the Go side:

- `tests/interop/python_client` against `bserve`: 135 checks pass, including a SHA-256 comparison of a binary file (log in `tests/interop/logs`).
- `bcurl` against `tests/interop/python_server`: statuses, exit codes and a byte-identical binary body, plus eight requests on one connection (also logged).

Building them surfaced about seventy places where the first draft of the specification was ambiguous or silent. Each was resolved in the specification rather than in code, and the runs above are against the final text. A classmate's server can be checked with the same tests: `BHTTP_PEER=host:port BHTTP_PEER_ROOT=dir go test -run External ./internal/client`.

`examples/hexdump/annotated-hexdump.md` is one real exchange (a client fetching a 69-byte PNG with one custom request header), captured by a recording proxy sitting between the two programs. The annotation is generated from the captured bytes and checked against the client's own `-v` output and the served file.

## Design decisions worth knowing

- **Metadata and body are separate frames.** A response is a RESPONSE frame followed by DATA frames of at most 16 KiB, so memory stays bounded whatever the file size, and body bytes are never interpreted.
- **Two kinds of failure, two channels.** A well-framed but invalid request gets a `400` on its own stream and the connection carries on, because the frame boundary is still trustworthy. Anything that makes the stream untrustworthy (an oversized length, stream ID 0) gets an `ERROR` frame on stream 0 and the connection closes. Unknown frame types are skipped, checked right after the length so nothing else about them can cause a fault.
- **The length is validated before anything is allocated or read**, including for frame types we do not know.
- **After an `ERROR` the sender half-closes and drains for at most one second and 64 KiB.** Closing with unread input makes TCP send a reset, which can destroy the error frame before the peer reads it; the bounds stop a hostile peer from holding the connection open.
- **Empty bodies end on the RESPONSE frame, never on a trailing empty DATA frame.** A file whose size is an exact multiple of 16384 ends on its last full DATA frame. A file that turns out shorter than announced ends in an `ERROR`, never a clean end of stream, so a truncated download cannot look complete.
- **Path rules are the same on every platform** (no backslashes, colons, device names, trailing dots and so on), and the resolved path, including symlinks and junctions, is checked to stay inside the root as a second line of defence.

## Known limits

- The Go race detector could not be run: it needs a C compiler and this Windows machine has none. The concurrency is exercised by tests with many simultaneous connections, but not under `-race`.
- The last round of specification clarifications (path and UTF-8 details, links inside the root, reset versus EOF after an error) was not re-read by a fresh blind implementer. The interoperability runs above were made against the final text and pass, and the Go code already followed those rules, but that is a weaker check than a new independent reading.
- Interoperability has been shown against two implementations written from the specification, not yet against another person's server.
- There is no version field in the frame header. A different version can only be detected by a fault.
- Requests are sequential in v1. There is no multiplexing, request body, compression or TLS.
