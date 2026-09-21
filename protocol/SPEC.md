# BHTTP/1 — Binary HTTP Protocol, Version 1

Normative. Byte layouts: `WIRE_FORMAT.md`. Header IDs: `HEADER_TABLE.md`.
An implementer needs only these three files. The key words MUST, MUST NOT,
SHOULD, MAY are used as in RFC 2119.

BHTTP/1 is a project protocol. It is not HTTP and is not compatible with HTTP.

## 1. Purpose and scope

A small request/response protocol carried as binary frames over one persistent
TCP connection. A client asks for a resource by path; a server answers with a
status, headers and an opaque byte body. v1 defines one method (GET), file-serving
semantics, no request bodies, no multiplexing, no compression, no TLS.

## 2. Transport and connection model

1. Transport is TCP. The server listens on a port chosen by its operator; there
   is no assigned port.
2. The connection is persistent. After a response completes (§5), the same
   connection carries the next request.
3. TCP read and write boundaries have no meaning. A read may return part of a
   frame, one frame, several frames, or a frame plus part of the next.
   Receivers MUST reassemble frames from the byte stream (exact reads);
   senders MUST ensure every byte of a frame is written.
4. A client MUST NOT open a second connection merely to send another request
   and MUST NOT reconnect silently.
5. Byte order is big-endian everywhere.

## 3. Frames

Every frame is a 12-byte header plus `Length` payload bytes
(`WIRE_FORMAT §2`). Maximum payload is **16384** bytes. Types:

```text
0x01 REQUEST   0x02 RESPONSE   0x03 DATA   0x04 ERROR   others: unknown
```

Flag: `END_STREAM = 0x01`. All other flag bits are reserved and MUST be 0 on
known frame types. Reserved (16 bits): sender MUST send 0; receiver MUST
ignore it.

Undefined flag bits on receipt:

| Frame, receiver | Behavior |
|---|---|
| REQUEST at a server | `RESPONSE 400` (§8.7) |
| RESPONSE or DATA at a client | Ignore the undefined bits (forward compatibility) |
| ERROR | Ignore |
| Unknown type | Not examined (§4.3) |

Servers are strict so that malformed clients are caught deterministically;
clients are lenient so a newer server can set new flags without breaking them.

## 4. Receiver processing order

A receiver MUST process each frame in this order (`WIRE_FORMAT §10`):

1. Read exactly 12 bytes.
2. If `Length > 16384`: connection-level fault `FRAME_TOO_LARGE` (§9). If the
   frame's type is ERROR, just close (an ERROR is never answered, §9.1).
3. If the type is **unknown**: read and discard exactly `Length` bytes and
   continue with the next frame. Flags, Reserved and Stream ID of an unknown
   frame MUST NOT be examined. Unknown frames MUST NOT close the connection.
4. Read exactly `Length` payload bytes.
5. If the type is REQUEST, RESPONSE or DATA and Stream ID is 0:
   connection-level fault `INVALID_STREAM_ID` (§9).
6. Validate flags and payload for the type (§6–§8). Act.

The payload of a known frame is never parsed before its boundary is established.

This order applies to **both roles**. Step 5 precedes step 6, so a RESPONSE or
DATA with Stream ID 0 arriving at a server is `ERROR(2)`, not `400` (§8.8
applies only to non-zero Stream IDs). A client that detects a fault in steps 2
or 5 sends ERROR and closes, exactly as a server does.

## 5. Streams and exchanges

1. A stream is one request/response exchange, identified by a Stream ID.
2. The client uses `1, 2, 3, …`, one new ID per request, in increasing order.
   A server MUST accept any non-zero ID and MAY ignore ordering or reuse.
   Every REQUEST a client sends consumes an ID, even one answered `400`. A
   client MUST NOT wrap around: once ID 4294967295 has been used, it MUST NOT
   send another request; it reports failure and closes the connection.
3. RESPONSE and DATA frames carry the Stream ID of their REQUEST.
4. Exchange shape:

```text
client: REQUEST  stream=n  END_STREAM
server: RESPONSE stream=n  status=…            (END_STREAM iff body is empty)
server: DATA     stream=n  …                   (zero or more)
server: DATA     stream=n  … END_STREAM        (last frame, iff body non-empty)
```

5. A stream ends when END_STREAM is seen on it. The connection then remains
   open (§10).
6. Requests are sequential in v1. A client SHOULD wait for END_STREAM before
   sending the next request. If a client pipelines anyway, the server MUST
   process requests in the order received, and frames of different streams
   MUST NOT be interleaved.
7. Empty vs. non-empty body:
   - Empty body: END_STREAM is on the RESPONSE frame; no DATA frame follows.
   - Non-empty body: RESPONSE has no END_STREAM; one or more DATA frames follow;
     the last carries END_STREAM. A sender MUST NOT send a trailing empty DATA
     frame, including when the size is an exact multiple of 16384.
   - A receiver MUST tolerate an empty DATA frame with END_STREAM, and MUST
     tolerate an empty DATA frame without END_STREAM (it adds 0 bytes). A
     sender SHOULD NOT produce either except as the case above.
8. The body is the concatenation of DATA payloads, in order. Bodies are opaque
   bytes and MUST NOT be decoded, normalized or terminated.

## 6. Frame types

### 6.1 REQUEST (0x01, client → server)

Layout: `WIRE_FORMAT §5`. Method `0x01` = GET (only defined method).
Flags: END_STREAM MUST be set (no request body in v1). The path is opaque
UTF-8 bytes of length 1–4096 with an explicit length; the file-serving rules for
it are in §11. Request headers are validated (§7, §8) and otherwise ignored by
a file server; none is required, and a client MAY send Header Count 0. A server never
validates the value of a request header, only its structure. The
smallest valid REQUEST payload is 5 bytes (method, Path Length, 1-byte path,
Header Count). A client is not required to validate the path before sending;
the server enforces §11.1. A REQUEST received by a client is a protocol fault:
the client sends `ERROR(3)` and closes.

### 6.2 RESPONSE (0x02, server → client)

Layout: `WIRE_FORMAT §6`. Status is `100–599`. v1 statuses emitted by a
server: `200`, `400`, `404`, `500`. END_STREAM per §5.7.

### 6.3 DATA (0x03, server → client)

Layout: `WIRE_FORMAT §8`. Carries body bytes. Clients MUST NOT send DATA in v1.

### 6.4 ERROR (0x04, either direction)

Layout: `WIRE_FORMAT §9`. Connection-level; see §9.

## 7. Headers

### 7.1 Encoding

Each header is a known header (ID ≠ 0, name implied by `HEADER_TABLE.md`) or a
custom header (ID = 0, name explicit). Layouts: `WIRE_FORMAT §7`.

### 7.2 Names

Senders MUST use the table ID for any table name. A custom header whose name
equals a table name is malformed. Custom names are 1–255 bytes of lowercase
`a-z 0-9 - _ .`; anything else is malformed.

### 7.3 Values, duplicates

Values are opaque bytes; only `content-length` has a defined syntax (decimal
digits). The same header may repeat; order is preserved; v1 defines no merging.

### 7.4 Unknown IDs

A header with a non-zero ID that the receiver does not know (IDs 9–255 in v1)
MUST be parsed with the known-header layout and ignored. It is not an error.

## 8. Malformed requests → `400`

A REQUEST is malformed, and the server MUST answer `RESPONSE 400` on the same
stream and keep the connection open, if any of these holds:

1. Method ≠ 0x01.
2. Payload shorter than the fixed fields, or Path Length is 0, exceeds 4096, or
   exceeds the bytes remaining.
3. Header Count promises more headers than the payload holds, or a header runs
   past the end of the payload.
4. Bytes remain after the last header.
5. Path is not valid UTF-8, or violates §11.1.
6. A custom header name is invalid or equals a table name (§7.2).
7. An undefined flag bit is set on the REQUEST, or END_STREAM is not set.
8. The frame is a RESPONSE or DATA received by a server.

A `400` response has an empty body (§11.4). The malformed frame's boundary is
known, so the stream stays synchronized; that is why this is not a connection
fault. Every offending frame gets its own `400`, even when the stream was
already answered, so a client that sends several bad frames receives several
`400`s.

## 9. Connection-level faults: ERROR

Some faults leave the receiver unable to attribute the problem to a valid
stream, or unable to trust the byte stream. These use ERROR on Stream ID 0,
after which the sender MUST close the connection.

| Fault | Code | Sender action |
|---|---|---|
| `Length > 16384` in any frame | 1 `FRAME_TOO_LARGE` | send ERROR, close |
| REQUEST/RESPONSE/DATA with Stream ID 0 | 2 `INVALID_STREAM_ID` | send ERROR, close |
| Other unrecoverable fault, e.g. a failure while streaming a body after RESPONSE was sent | 3 `PROTOCOL_ERROR` | send ERROR, close |

Rules:

1. A receiver of ERROR MUST treat the connection as failed and close it. It MUST
   NOT reply, whatever the ERROR's length, flags or contents. An unknown code, a
   short payload or invalid UTF-8 in Msg does not change this (a display MAY
   substitute replacement characters).
2. If a client receives ERROR or EOF before END_STREAM of an outstanding
   stream, that response is incomplete and the client MUST report failure.
3. Delivery of an ERROR is best effort, but the sender MUST shut down its write
   side (a TCP FIN) after sending it rather than abort the connection. It
   SHOULD then read and discard incoming bytes, bounded in both time and volume
   (reference: 1 s from the moment the ERROR was sent, and 64 KiB), before
   fully closing. Closing with unread input pending can make the operating
   system reset the connection, and a reset can destroy the ERROR before the
   peer has read it.
4. A peer that has just sent an ERROR MUST treat a reset the same as EOF. While
   a stream is outstanding, a receiver that sees EOF or a reset without an
   ERROR treats it as failure too; EOF at a frame boundary with no stream
   outstanding is a clean close (§10.2).
5. A peer that speaks the wrong protocol (for example text HTTP, whose first
   four bytes read as a length far above 16384) is handled by the first row of
   the table above.

The three response classes are therefore:

| Situation | Behavior |
|---|---|
| Valid frame, invalid request contents | `RESPONSE 400`, same stream, continue |
| Cannot safely interpret frame or stream | `ERROR`, stream 0, close |
| Unknown frame type | Skip by length, continue |

Truncation (EOF inside a header or payload) sends nothing: the partial frame is
discarded and the connection closed.

## 10. Connection lifetime

1. After END_STREAM, the server waits for the next frame.
2. A peer ends the connection by closing TCP. EOF at a frame boundary is a clean
   close; EOF inside a frame is truncation (§9).
3. No header controls connection lifetime. `connection` (ID 5) has no meaning in v1.
4. Idle timeouts and per-connection limits are implementation policy (§14), not
   wire behavior.

## 11. File-serving semantics

`bserve <root> <port>` serves files below the directory `<root>`.

### 11.1 Path validity (→ `400` if violated)

The path MUST satisfy **all** of the following. Servers apply these rules on
every platform so behavior is identical everywhere.

1. Begins with `/`.
2. Contains no byte below `0x20`, no `0x7F`, no NUL.
3. Contains none of `\ : * ? " < > |`.
4. Contains no `//`.
5. No segment (text between slashes) is `.` or `..`.
6. No segment ends with `.` or a space.
7. No segment, ignoring case and any extension (text from the first `.`), is a
   reserved device name: `CON PRN AUX NUL COM1–COM9 LPT1–LPT9`.

Notes on the rules. "Valid UTF-8" means RFC 3629: no overlong forms, no
surrogate code points, nothing above U+10FFFF; other non-ASCII characters are
allowed. A trailing slash leaves an empty final segment, which is allowed;
empty segments anywhere else are excluded by rule 4. For rule 7 the name that
is compared is the text before the first `.`, so `/nul.tar.gz`, `/CON.txt` and
`/dir/com1` are rejected, while `/.hidden`, `/console`, `/com10` and `/x.con`
are fine.

There is **no** percent-decoding and no query string: `?` is rejected by
rule 3, while `%` and `#` are ordinary bytes. Path bytes are matched literally
against file names.

### 11.2 Mapping

The path minus its leading `/`, split on `/`, is joined to `<root>`.

```text
/                     → <root>/index.html
/index.html           → <root>/index.html
/css/style.css        → <root>/css/style.css
/docs/  or  /docs     → <root>/docs/index.html   (if /docs is a directory)
```

A path that resolves to a directory serves `index.html` from it, with or
without a trailing slash. A path with a trailing slash that names a regular
file does not match (404).

### 11.3 Containment (defense in depth)

After the string rules, the server MUST resolve the final filesystem path,
including symbolic links and Windows junctions, and MUST NOT serve it unless it
lies inside the resolved `<root>`. A path that resolves outside the root is
answered `404`, and the server MUST NOT reveal whether anything exists outside
the root. A link that stays inside the root is served like any other file.
Whether a name matches case-sensitively is whatever the underlying filesystem
does and is not part of the protocol.

### 11.4 Responses

| Case | Response |
|---|---|
| Found, regular file | `200`, headers, body = exact file bytes |
| Missing file; directory without `index.html`; not a regular file; outside root | `404`, empty body |
| Malformed request or invalid path (§8, §11.1) | `400`, empty body |
| Internal failure before RESPONSE is sent | `500`, empty body |
| Internal failure after RESPONSE is sent, such as a file that turns out shorter than announced | `ERROR(3)`, close (§9) |

Headers a v1 file server sends, in this order:

- `200`: `content-type` (1), `content-length` (2), `server` (3).
- `400`, `404`, `500`: `content-length` (2) with value `0`, `server` (3).

`content-length` on a `200` MUST equal the total DATA bytes. `content-type` is
chosen from the file extension; unknown extension → `application/octet-stream`.
The extension table and the `server` value are informational and not part of
the protocol.

### 11.5 Body transmission

The file is read as raw bytes and split across DATA frames of at most 16384
bytes, in order. The server determines the size from the open file so
`content-length` matches the bytes sent. An empty file gives `200` with
END_STREAM on the RESPONSE (§5.7).

## 12. Receiving responses (client rules)

A client conforming to v1:

1. Follows §4 (skips unknown types; faults per §9).
2. Expects RESPONSE for its Stream ID, then DATA frames for the same stream.
3. Treats as a protocol failure: a RESPONSE/DATA with a different Stream ID,
   DATA before RESPONSE, a second RESPONSE on one stream, or any frame on a
   stream after its END_STREAM. The last case may be noticed lazily, when the
   stray frame is read during a later exchange.
4. Treats ERROR as connection failure (§9).
5. Considers the response complete only at END_STREAM. If `content-length` is
   present, every occurrence must be 1 to 19 ASCII digits whose numeric value
   (leading zeros allowed) equals the DATA total, otherwise the response is
   invalid.
6. Status: valid range is `100–599`, otherwise invalid. v1 has no interim
   responses: every RESPONSE is final. Status `≥ 400` is an error (a CLI exits
   non-zero); `100–399` is not an error, and 3xx is not followed.
7. Header parsing in a RESPONSE is structural only: unknown IDs are ignored
   (§7.4) and custom-name characters are not validated; a Name Length of 0 or
   above 255, or a header running past the payload, is malformed.
8. Any failure under items 3, 5, 6 or 7, or an ERROR, EOF or truncation while
   a stream is outstanding, ends the exchange as failed and the client closes
   the connection; it does not reuse a connection after a protocol failure and
   does not reconnect silently. The client sends an ERROR only for the faults
   in §4 steps 2 and 5 and for a REQUEST received (§6.1); for the failures in
   this list it just closes.
9. Timeouts are implementation policy (§14). What a CLI prints for a `≥ 400`
   body is outside the protocol.

## 13. Limits

```text
Frame payload            16384 bytes
Path                      4096 bytes
Headers per message        255
Custom header name         255 bytes
Header value             65535 by encoding, bounded by the frame
Whole REQUEST or RESPONSE payload  ≤ 16384 (it is one frame)
```

Every variable length MUST be checked against these limits and the remaining
payload before use. A message that cannot fit one frame cannot be sent.

## 14. Implementation policy (not wire behavior)

Not required for interoperability, but expected of `bserve`: handle connections
concurrently; close a connection idle beyond a timeout (reference: 30 s);
stream large files rather than loading them; clients bound how long they wait
for a response; keep error text out of ERROR
messages beyond a short generic reason.

## 15. Versioning and compatibility

v1 has no version field on the wire, so a v1 peer cannot detect a different
version other than by a fault. Future versions add capabilities through new
frame types (skipped by v1 peers) and by assigning Flags, Reserved and header
IDs. v1 receivers ignore Reserved and unknown header IDs precisely so those
extensions are safe.

Compatibility depends only on `SPEC.md`, `WIRE_FORMAT.md` and `HEADER_TABLE.md`:
no implementation structs, private headers, timing, or TCP packet boundaries.
