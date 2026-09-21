# BHTTP/1 Wire Format

Normative. Defines every byte on the wire. Meaning and behavior are in
`SPEC.md`; header IDs are in `HEADER_TABLE.md`. If this file and `SPEC.md`
disagree, that is a specification bug: report it, do not guess.

The key words MUST, MUST NOT, SHOULD, MAY are used as in RFC 2119.

## 1. Conventions

- Transport: TCP byte stream. Read/write boundaries carry no meaning.
- All multi-byte integers are unsigned, **big-endian** (network byte order).
- `u8`, `u16`, `u32` = 1, 2, 4-byte unsigned integers.
- Offsets are zero-based from the start of the structure being described.
- `bytes[N]` = exactly N raw bytes, no terminator, no padding.
- There is no padding or alignment between fields.

## 2. Frame

A frame is a 12-byte header immediately followed by `Length` payload bytes.
Frames are concatenated on the stream with nothing between them.

```text
Offset  Size  Field      Type  Notes
──────────────────────────────────────────────────────────────
0       4     Length     u32   payload bytes only; 0..16384
4       1     Type       u8    §4
5       1     Flags      u8    §4
6       2     Reserved   u16   sender MUST send 0; receiver MUST ignore
8       4     Stream ID  u32   §4

12      Length bytes payload
```

```text
 0        4    5    6        8            12
 ┌────────┬────┬────┬────────┬────────────┬───────────────┐
 │ Length │ T  │ F  │Reserved│ Stream ID  │ payload ...   │
 │  u32   │ u8 │ u8 │  u16   │    u32     │ Length bytes  │
 └────────┴────┴────┴────────┴────────────┴───────────────┘
```

## 3. Length

- `Length` is the number of payload bytes after the 12-byte header. The
  header is not counted.
- `Length` MUST be in `0 … 16384` (`0x00000000 … 0x00004000`).
- A frame whose `Length` field is greater than 16384 is a connection-level
  fault (`FRAME_TOO_LARGE`, SPEC §9). This applies to **every** frame,
  including frames of unknown type. If the oversize frame is an ERROR, the
  receiver just closes (ERROR is never answered).
- A receiver MUST NOT allocate memory based on `Length` before checking it
  against 16384.

## 4. Type, Flags, Stream ID

```text
Type   Name      Direction        Payload
──────────────────────────────────────────────────────────
0x01   REQUEST   client → server  §5
0x02   RESPONSE  server → client  §6
0x03   DATA      server → client  §8   (v1: never sent by clients)
0x04   ERROR     either           §9
other  unknown   —                opaque; skipped by length
```

```text
Flags bit  Mask  Name
───────────────────────────────
0          0x01  END_STREAM
1–7        0xFE  reserved; MUST be 0 on known frame types
```

- END_STREAM: this is the final frame of the stream.
- On **unknown** types, Flags, Reserved and Stream ID have no defined meaning
  and MUST NOT be checked.
- On **ERROR**, Flags MUST be sent as 0 and are ignored on receipt.

Stream ID:

- REQUEST, RESPONSE, DATA: non-zero. Value 0 is a connection-level fault
  (`INVALID_STREAM_ID`).
- ERROR: sender MUST use 0; receiver ignores the value.
- Client stream IDs are `1, 2, 3, …`, one new ID per request.
- RESPONSE and DATA carry the Stream ID of the REQUEST they answer.

## 5. REQUEST payload

```text
Offset  Size          Field         Type
────────────────────────────────────────────────
0       1             Method        u8     0x01 = GET
1       2             Path Length   u16    1..4096
3       Path Length   Path          bytes  UTF-8, no terminator
3+P     1             Header Count  u8     0..255
4+P     variable      Headers       §7 × Header Count
```

where `P` = Path Length. The payload MUST end exactly after the last header:
no bytes before, between or after the fields above are permitted.
REQUEST frames have no body.

## 6. RESPONSE payload

```text
Offset  Size      Field         Type
────────────────────────────────────────────
0       2         Status        u16   100..599
2       1         Header Count  u8    0..255
3       variable  Headers       §7 × Header Count
```

The payload MUST end exactly after the last header.

Examples of the integer encoding: `200 = 00 C8`, `400 = 01 90`,
`404 = 01 94`, `500 = 01 F4`.

## 7. Header encoding

Each header is one of two forms, selected by the first byte (Header ID).

Known header (ID 1–255):

```text
Offset  Size          Field         Type
──────────────────────────────────────────
0       1             Header ID     u8    ≠ 0
1       2             Value Length  u16
3       Value Length  Value         bytes
```

Custom header (ID = 0):

```text
Offset  Size          Field         Type
──────────────────────────────────────────
0       1             Header ID     u8    = 0
1       2             Name Length   u16   1..255
3       Name Length   Name          bytes lowercase ASCII, see below
3+N     2             Value Length  u16
5+N     Value Length  Value         bytes
```

where `N` = Name Length.

- The first byte alone decides the form: 0 ⇒ custom, non-zero ⇒ known. A
  parser therefore always knows where a header ends, even for an ID it does
  not recognize (IDs 9–255 use the known layout).
- Custom Name bytes: `a`–`z`, `0`–`9`, `-`, `_`, `.` only.
- Value Length is `0 … 65535` in the encoding, but a header must fit inside
  its frame, so it is in practice bounded by 16384.
- Values are opaque bytes. The header table (`HEADER_TABLE.md`) states which
  values have a defined syntax.
- Headers are read sequentially; Header Count says how many. Reading fewer or
  more bytes than the payload holds is malformed.

## 8. DATA payload

The payload is the body bytes, unmodified. No length prefix, no encoding, no
terminator. `Length` may be 0 … 16384. The body is the concatenation, in
order, of the payloads of all DATA frames of the stream.

## 9. ERROR payload

```text
Offset  Size      Field    Type
─────────────────────────────────────────
0       2         Code     u16
2       2         Msg Len  u16
4       Msg Len   Msg      bytes  UTF-8, diagnostic text
```

```text
Code  Name
──────────────────────────────
1     FRAME_TOO_LARGE
2     INVALID_STREAM_ID
3     PROTOCOL_ERROR
```

- The payload MUST end exactly after Msg. A malformed or unknown-code ERROR
  is still an ERROR: the receiver treats it as a connection-level fault.
- Msg is for humans. A receiver MUST NOT act on its contents. A sender MUST
  NOT put filesystem paths or other sensitive detail in it. `Msg Len = 0` is
  permitted.
- Stream ID is 0, Flags are 0.

## 10. Frame reader algorithm (normative behavior, both roles)

```text
loop:
  read exactly 12 bytes                  EOF at 0 bytes  → clean end, stop
                                         EOF after 1–11  → truncated, discard, stop
  Length = u32(bytes 0..3)
  Type   = byte 4
  if Length > 16384:
      if Type == ERROR                   → close
      else                               → send ERROR(FRAME_TOO_LARGE), close
  if Type not in {1,2,3,4}:              read exactly Length bytes, discard, continue
                                         (EOF early → truncated, stop)
  read exactly Length bytes              EOF early → truncated, discard, stop
  if Type in {1,2,3} and StreamID == 0   → send ERROR(INVALID_STREAM_ID), close
  dispatch:
    ERROR                                → close, send nothing
    server role: REQUEST → SPEC §8/§11 ; RESPONSE, DATA → RESPONSE 400 on that stream
    client role: RESPONSE, DATA → SPEC §12 ; REQUEST → send ERROR(PROTOCOL_ERROR), close
```

The unknown-type test happens **before** any check of Flags, Reserved or
Stream ID. Reserved is never checked.

## 11. Outcome matrix

Server receiving a frame. "Continue" means the connection stays open and the
next frame is read.

| Condition | Outcome |
|---|---|
| EOF exactly at a frame boundary | Clean close |
| EOF inside header or payload | Discard partial frame; close; nothing sent |
| `Length > 16384`, type not ERROR (including unknown types) | `ERROR(1)` on stream 0, close |
| `Length > 16384`, type ERROR | Close, no reply |
| Unknown type, `Length ≤ 16384` | Skip payload; continue |
| REQUEST / RESPONSE / DATA with Stream ID 0 | `ERROR(2)` on stream 0, close |
| ERROR received (any length, flags, contents) | Close (no reply) |
| RESPONSE or DATA received by a server | `RESPONSE 400` on that stream; continue |
| REQUEST, undefined Flags bit set | `RESPONSE 400`; continue |
| REQUEST, END_STREAM not set | `RESPONSE 400`; continue |
| REQUEST payload malformed (SPEC §8) | `RESPONSE 400`; continue |
| REQUEST path violates SPEC §11.1 | `RESPONSE 400`; continue |
| REQUEST valid, file missing / directory without index / escapes root | `RESPONSE 404`; continue |
| Failure before RESPONSE is sent (e.g. cannot open file) | `RESPONSE 500`; continue |
| Failure after RESPONSE was sent (e.g. read error mid-body) | `ERROR(3)`, close |
| REQUEST valid, file found | `RESPONSE 200` + DATA…; continue |

## 12. Rationale (informative)

Strictness differs by role on purpose: a server rejects undefined Flags bits in
REQUEST (`400`) so faulty clients are caught deterministically, a client ignores
them in RESPONSE/DATA so a newer server can add flags. Reserved (16 bits) is
ignored by everyone because it has no defined content at all.

Field widths:

| Field | Width | Why |
|---|---|---|
| Length | u32 | Payload cap is 2^14, so 14 bits suffice today. 32 bits keeps the header 4-byte aligned and lets a future revision raise the cap without changing the header layout. |
| Type | u8 | 4 used, 251 left for extensions; unknown types are skippable. |
| Flags | u8 | One flag used; seven reserved with defined behavior. |
| Reserved | u16 | Room for a future per-frame field. Sending 0 / ignoring on receipt means old peers stay compatible. Brings the header to 12 bytes. |
| Stream ID | u32 | Distinguishes exchanges on one connection; 2^32 exchanges per connection is ample. 0 reserved for connection-level ERROR. |
| Path Length | u16 | Cap 4096 fits; u8 would not. |
| Header Count | u8 | 255 headers is far above need. |
| Header Value Length | u16 | Wider than a frame allows (16384) but keeps a simple, uniform field. |
| Header ID | u8 | 0 = custom, 1–255 table IDs. |
