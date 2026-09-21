# Testing your BHTTP/1 implementation against this one

This guide is for someone who has written (or is writing) a BHTTP/1 client and wants to know, without guessing, whether it interoperates with this server. It also covers the reverse: checking your server with the tools here.

You need three things: the protocol (`protocol/bhttp1-spec.pdf`, two pages, with `SPEC.md`, `WIRE_FORMAT.md` and `HEADER_TABLE.md` as the full text), a server to point your client at (section 1), and something that tells you what "correct" looks like (sections 2 to 5). Everything below has been run for real; nothing in it is aspirational.

- [1. Get a server](#1-get-a-server)
- [2. The fixture: what the server serves](#2-the-fixture-what-the-server-serves)
- [3. Conformance checklist for a client](#3-conformance-checklist-for-a-client)
- [4. Stress your framing with bchaos](#4-stress-your-framing-with-bchaos)
- [5. Compare your bytes with known-good exchanges](#5-compare-your-bytes-with-known-good-exchanges)
- [6. Testing a server instead](#6-testing-a-server-instead)
- [7. When something does not match](#7-when-something-does-not-match)
- [8. Mistakes worth avoiding](#8-mistakes-worth-avoiding)

## 1. Get a server

Any one of these gives you a server on port 9000 serving the fixture folder `conformance/www`.

```bash
# Docker: nothing else to install, works on any OS (image is about 10 MB)
docker build -t bhttp .
docker run --rm -p 9000:9000 bhttp

# Go 1.23 or newer
go build -o bin/ ./cmd/bserve ./cmd/bcurl ./cmd/bchaos
bin/bserve conformance/www 9000
```

The server logs one line per connection and per request, which is your evidence for what your client really did:

```
2026/09/21 19:30:14 172.17.0.1:40372 connected
2026/09/21 19:30:14 GET "/edge-65536.bin" -> 200 (65536 bytes)
2026/09/21 19:30:14 172.17.0.1:40372 disconnected
```

A client that sends ten requests on one connection produces one `connected` line, ten `GET` lines and one `disconnected` line. Two `connected` lines for one run means your client reconnected, which the protocol forbids.

`bcurl` is a reference client you can compare against: `bin/bcurl -v localhost:9000/index.html` prints every frame in both directions, from the bytes actually read and written.

## 2. The fixture: what the server serves

Eleven files, chosen so that every case a client can get wrong has a file that exposes it. The bytes are deterministic and checked in `conformance/SHA256SUMS` (verify with `cd conformance && sha256sum -c SHA256SUMS`, or `Get-FileHash` in PowerShell). Every path below is also covered by an automated test that fetches it through the server and compares its SHA-256 and its frame count with this table.

| Path | Bytes | DATA frames | Content-type | Why it is there |
|---|---|---|---|---|
| `/` | 115 | 1 | text/html | maps to `index.html` |
| `/index.html` | 115 | 1 | text/html | the plain case |
| `/docs` and `/docs/` | 97 | 1 | text/html | a directory serves its `index.html`, with or without the slash |
| `/assets/site.css` | 54 | 1 | text/css | a nested path |
| `/unicode/naïve.txt` | 11 | 1 | text/plain | the path is UTF-8 on the wire (`6E 61 C3 AF 76 65`) |
| `/binary.bin` | 261 | 1 | application/octet-stream | every byte value 0x00 to 0xFF, then `0D 0A 00 0D 0A`: catches anything that treats the body as text |
| `/pixel.png` | 69 | 1 | image/png | a real image |
| `/empty.txt` | 0 | 0 | text/plain | an empty body ends on the RESPONSE frame |
| `/edge-16383.bin` | 16383 | 1 | application/octet-stream | one byte under a full frame |
| `/edge-16384.bin` | 16384 | 1 | application/octet-stream | exactly one full frame: no empty DATA frame follows |
| `/edge-16385.bin` | 16385 | 2 | application/octet-stream | 16384 then 1: the second frame carries END_STREAM |
| `/edge-65536.bin` | 65536 | 4 | application/octet-stream | four full frames, END_STREAM on the fourth |

The `edge-*` files repeat with a period of 251, which never lines up with a frame boundary, so a client that drops, repeats or reorders a byte at a boundary changes the checksum.

## 3. Conformance checklist for a client

Run these against your client, in order, on **one connection** where the client allows it. "Expected" is what the server sends and what your client should report. A ✔ means you can tick it off.

**Bodies and framing**

| # | Send | Expected |
|---|---|---|
| 1 | `GET /` and `GET /index.html` | 200, 115 bytes, identical to each other; headers `content-type: text/html`, `content-length: 115`, `server: bserve/1` |
| 2 | `GET /docs`, `GET /docs/` | 200, 97 bytes each |
| 3 | `GET /assets/site.css` | 200, 54 bytes, `text/css` |
| 4 | `GET /unicode/naïve.txt` | 200, 11 bytes (your client must send the path as UTF-8 bytes, not percent-encoded) |
| 5 | `GET /binary.bin` | 200, 261 bytes, SHA-256 matches; nothing changed by newline handling or NUL bytes |
| 6 | `GET /edge-16383.bin` | one DATA frame of 16383 bytes carrying END_STREAM |
| 7 | `GET /edge-16384.bin` | one DATA frame of 16384 bytes carrying END_STREAM, and no empty frame after it |
| 8 | `GET /edge-16385.bin` | DATA of 16384, then DATA of 1 with END_STREAM |
| 9 | `GET /edge-65536.bin` | four DATA frames of 16384, END_STREAM on the last; SHA-256 matches |
| 10 | `GET /empty.txt` | 200, 0 bytes; the RESPONSE frame itself carries END_STREAM and no DATA frame follows |

**Errors** (each must leave the connection usable for the next request)

| # | Send | Expected |
|---|---|---|
| 11 | `GET /nope` | 404, empty body, RESPONSE with END_STREAM, `content-length: 0` |
| 12 | `GET /a:b`, `GET /../x`, `GET /a\b`, `GET //x`, `GET /con`, `GET /index.html.` | 400 each, empty body. Your client must be able to send these paths unmodified. A client may refuse to send `index.html` (no leading slash) locally; if it does send it, expect 400. |
| 13 | any request after a 400 or 404 | works on the same connection |
| 14 | a CLI client: exit status after 404 and 400 | non-zero |

**Connection behaviour**

| # | Check | Expected |
|---|---|---|
| 15 | ten requests in a row | stream IDs 1 to 10 on the wire, one `connected` line in the server log |
| 16 | your client reads only what it needs | after the last frame of a response it stops reading; it does not wait for the connection to close |
| 17 | run 1 to 10 through `bchaos` (next section) | bodies and checksums unchanged |

## 4. Stress your framing with bchaos

Most client bugs are invisible on a fast local connection, because TCP tends to hand over whole frames. `bchaos` is a proxy that removes that luck. Put it between your client and the server:

```
your client  ->  bchaos (:9100)  ->  bserve (:9000)
```

```bash
bin/bchaos -target 127.0.0.1:9000 -listen 127.0.0.1:9100 -chunk 1              # one byte at a time
bin/bchaos -target 127.0.0.1:9000 -listen 127.0.0.1:9100 -chunk 7 -delay 1ms   # random pieces of 1 to 7 bytes
bin/bchaos -target 127.0.0.1:9000 -listen 127.0.0.1:9100 -noise                # unknown frame types injected
bin/bchaos -target 127.0.0.1:9000 -listen 127.0.0.1:9100 -chunk 5 -noise -seed 42
```

- **`-chunk n`** writes the server's answer to your client in random pieces of 1 to n bytes. A client that assumes one `read` returns a whole frame header, or a whole frame, fails here. `-delay` pauses after each piece so the operating system does not merge the pieces back together; on some systems a delay of even a few microseconds lasts a millisecond, so keep it small for large files.
- **`-noise`** inserts frames of unknown types (5, 6, 10, 126, 128, 254, 255) with random flags, reserved bytes, stream IDs (including 0) and payloads, before every server frame and after the last frame of each response. A correct client skips them by length and never looks at their other fields. A client that treats an unknown type as an error, or that checks the stream ID before the type, fails here.
- **`-seed n`** makes a failing run repeatable.

The proxy leaves requests untouched and passes ERROR frames and closes through. Its own tests prove it catches two classic mistakes (assuming one read is one frame header, and rejecting unknown types) while a correct client passes every mode. If your client passes checklist items 1 to 10 directly and fails through the proxy, the bug is in how it reads.

The server closes a connection that has been idle for 30 seconds, so do not slow the proxy so much that one response takes longer than that.

## 5. Compare your bytes with known-good exchanges

These are real captures of `bcurl -v` against this server (`stream=1`, no request headers). If your client sends no headers, your request should be **byte for byte identical** to the request shown. If you add headers, only the header block and the length fields may differ. The DATA payload is the file itself and is left out where it is long.

**`GET /index.html`**

```
--> frame 1  REQUEST  stream=1  flags=END_STREAM  length=15
    header   00 00 00 0F 01 01 00 00 00 00 00 01
    payload
      00000000  01 00 0B 2F 69 6E 64 65  78 2E 68 74 6D 6C 00     |.../index.html.|
<-- frame 2  RESPONSE  stream=1  flags=none  length=32
    header   00 00 00 20 02 00 00 00 00 00 00 01
    payload
      00000000  00 C8 03 01 00 09 74 65  78 74 2F 68 74 6D 6C 02  |......text/html.|
      00000010  00 03 31 31 35 03 00 08  62 73 65 72 76 65 2F 31  |..115...bserve/1|
<-- frame 3  DATA  stream=1  flags=END_STREAM  length=115
    header   00 00 00 73 03 01 00 00 00 00 00 01
    (115 bytes: the file)
```

Reading the request: `00 00 00 0F` length 15, `01` REQUEST, `01` END_STREAM, `00 00` reserved, `00 00 00 01` stream 1, then the payload `01` GET, `00 0B` path length 11, the 11 path bytes, `00` header count zero.

Reading the response: `00 C8` status 200, `03` three headers; header 1 is ID `01` (content-type) with a 9-byte value; header 2 is ID `02` (content-length) with the 3-byte value `115`; header 3 is ID `03` (server) with an 8-byte value. No header name is sent for any of them.

**`GET /empty.txt`**: the RESPONSE carries END_STREAM (`flags=END_STREAM`, byte `01` at offset 5) and nothing follows it.

```
<-- frame 2  RESPONSE  stream=1  flags=END_STREAM  length=31
    header   00 00 00 1F 02 01 00 00 00 00 00 01
    payload
      00000000  00 C8 03 01 00 0A 74 65  78 74 2F 70 6C 61 69 6E  |......text/plain|
      00000010  02 00 01 30 03 00 08 62  73 65 72 76 65 2F 31     |...0...bserve/1|
```

**`GET /nope`**: status `01 94` (404), two headers, END_STREAM on the RESPONSE.

```
<-- frame 2  RESPONSE  stream=1  flags=END_STREAM  length=18
    header   00 00 00 12 02 01 00 00 00 00 00 01
    payload
      00000000  01 94 02 02 00 01 30 03  00 08 62 73 65 72 76 65  |......0...bserve|
      00000010  2F 31                                             |/1|
```

A complete exchange with a custom request header (`x-demo: hello`), annotated byte by byte and generated from a recording of the real connection, is in `examples/hexdump/annotated-hexdump.md`. To capture your own client's bytes, put the recording proxy in the middle: `python examples/hexdump/capture_proxy.py 9101 127.0.0.1 9000 mycapture`, point your client at port 9101, then `python examples/hexdump/annotate.py mycapture.request.bin mycapture.response.bin > annotated.md`.

## 6. Testing a server instead

If you wrote a server, serve a copy of `conformance/www` and use the same table as your expected results.

```bash
# the reference client, one request at a time
bin/bcurl -v localhost:PORT/edge-16385.bin > out.bin
echo $?                       # 0 for 2xx/3xx, 1 for 4xx/5xx, 2 bad usage, 3 exchange failed

# the independent Python conformance runner: 135 checks on real connections
python tests/interop/python_client/run_interop.py HOST PORT --www conformance/www

# eight requests on one persistent connection, checked in Go (BHTTP_PEER_ROOT must be an absolute path)
BHTTP_PEER=HOST:PORT BHTTP_PEER_ROOT=/abs/path/to/conformance/www go test -run External ./internal/client
```

The Python runner covers: `/`, `/index.html` and `/binary.bin` with SHA-256 comparison against the files in `--www` (plus a missing file), header IDs and order, `content-length` against the DATA total, END_STREAM placement, stream ID echo, malformed requests (bad method, no END_STREAM, undefined flag bit, trailing byte, path traversal) each followed by a valid request on the same connection, an unknown frame type before a request, non-zero Reserved bytes, a request on stream 0 (expects `ERROR` code 2 then a close), and a frame declaring a payload over 16384 (expects `ERROR` code 1 then a close). It prints a PASS or FAIL line per check and exits non-zero on any failure.

Two of its checks hold you to the letter of the specification, not just to reasonable behaviour: an `ERROR` must be readable before the connection ends (the sender half-closes and drains before closing), and a server must answer each bad frame with its own `400`.

## 7. When something does not match

Do not change your implementation, or ours, straight away. Decide which of three things happened:

1. **The specification is ambiguous or silent.** Two careful readers could disagree. This has happened: the two independent implementations built for this project turned up about seventy such places, and each was fixed in the specification. If you find another, that is a real result; send the section, the two readings and the bytes.
2. **Your implementation breaks a rule.** Find the rule in `SPEC.md` and the wire layout in `WIRE_FORMAT.md`.
3. **This implementation breaks a rule.** Also a real result. Send the request bytes and the response bytes.

Bytes settle arguments. Capture the exchange with `bcurl -v` for our side, or with the recording proxy for yours, and compare frame by frame: length, type, flags, stream ID, then the payload fields.

## 8. Mistakes worth avoiding

These are the points where independent implementations stumbled while this specification was being written.

- **One read is not one frame.** Read exactly 12 bytes, then exactly `Length` bytes, in a loop. `bchaos -chunk 1` finds this instantly.
- **Skip unknown frame types by length,** before looking at flags, reserved bytes or the stream ID. They can appear anywhere.
- **Check `Length` against 16384 before allocating.** A hostile length must not cost you memory.
- **An empty body ends on the RESPONSE frame.** There is no empty DATA frame to wait for. A body that is an exact multiple of 16384 also has no trailing empty frame.
- **Do not reconnect to send the next request,** and use stream IDs 1, 2, 3 and so on, one per request.
- **Send a REQUEST with END_STREAM set.** Without it a server answers 400.
- **Table header names are sent by ID, never as custom headers,** and custom names are lowercase.
- **Verify `content-length` against what you received.** A mismatch means the response is invalid.
- **An `ERROR` frame means close.** Never reply to one.
- **Path bytes go on the wire as they are:** UTF-8, no percent-encoding, no trailing NUL.

## Quick reference

```
frame   = Length u32 | Type u8 | Flags u8 | Reserved u16 | StreamID u32 | payload[Length]     (big-endian, Length <= 16384)
types   = 1 REQUEST  2 RESPONSE  3 DATA  4 ERROR      flag 0x01 = END_STREAM
REQUEST = Method u8(1=GET) | PathLen u16 | Path | HeaderCount u8 | headers
RESPONSE= Status u16 | HeaderCount u8 | headers
header  = ID u8 (1..255) | ValueLen u16 | Value            known: 1 content-type 2 content-length 3 server 4 date
        | 0 | NameLen u16 | Name | ValueLen u16 | Value    custom                 5 connection 6 content-encoding 7 cache-control 8 last-modified
ERROR   = Code u16 (1 too large, 2 bad stream id, 3 protocol) | MsgLen u16 | Msg      always stream 0; then close
```
