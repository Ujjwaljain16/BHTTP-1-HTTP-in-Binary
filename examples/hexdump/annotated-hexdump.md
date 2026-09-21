# BHTTP/1 annotated hexdump

One complete exchange captured from a real run: `bcurl` fetching `/pixel.png` from `bserve`, with a recording proxy between them copying the bytes without looking at them. The request carries one custom header (`x-demo: hello`) so every header form appears in the capture. Offsets are counted from the first byte the sender transmitted in each direction.

Every multi-byte integer is big-endian. A frame is a 12-byte header (Length 4, Type 1, Flags 1, Reserved 2, Stream ID 4) followed by Length payload bytes, and the next frame starts immediately after.

## Request (client to server)

Raw bytes (42):

```text
0000  00 00 00 1E 01 01 00 00 00 00 00 01 01 00 0A 2F  |.............../|
0010  70 69 78 65 6C 2E 70 6E 67 01 00 00 06 78 2D 64  |pixel.png....x-d|
0020  65 6D 6F 00 05 68 65 6C 6C 6F                    |emo..hello|
```

Frame boundaries:

| Frame | Offsets | Type | Total bytes |
|---|---|---|---|
| 1 | `0000`-`0029` | REQUEST | 42 |

### Frame 1: REQUEST, client to server (42 bytes at offset 0x0000)

| Offset | Bytes | Field | Value |
|---|---|---|---|
| `0000` | `00 00 00 1E` | Length | 30 payload bytes (header not counted) |
| `0004` | `01` | Type | 0x01 = REQUEST |
| `0005` | `01` | Flags | 0x01 = END_STREAM |
| `0006` | `00 00` | Reserved | 0 (always zero when sent) |
| `0008` | `00 00 00 01` | Stream ID | 1 |
| `000C` | `01` | Method | 0x01 = GET |
| `000D` | `00 0A` | Path Length | 10 |
| `000F` | `2F 70 69 78 65 6C 2E 70 6E 67` | Path | "/pixel.png" |
| `0019` | `01` | Header Count | 1 |
| `001A` | `00` | Header 1: ID | 0 = custom header, name follows |
| `001B` | `00 06` | Header 1: Name Length | 6 |
| `001D` | `78 2D 64 65 6D 6F` | Header 1: Name | "x-demo" |
| `0023` | `00 05` | Header 1: Value Length | 5 |
| `0025` | `68 65 6C 6C 6F` | Header 1: Value | "hello" |

## Response (server to client)

Raw bytes (124):

```text
0000  00 00 00 1F 02 00 00 00 00 00 00 01 00 C8 03 01  |................|
0010  00 09 69 6D 61 67 65 2F 70 6E 67 02 00 02 36 39  |..image/png...69|
0020  03 00 08 62 73 65 72 76 65 2F 31 00 00 00 45 03  |...bserve/1...E.|
0030  01 00 00 00 00 00 01 89 50 4E 47 0D 0A 1A 0A 00  |........PNG.....|
0040  00 00 0D 49 48 44 52 00 00 00 01 00 00 00 01 08  |...IHDR.........|
0050  02 00 00 00 90 77 53 DE 00 00 00 0C 49 44 41 54  |.....wS.....IDAT|
0060  78 DA 63 B8 D6 A5 0D 00 03 C5 01 8C 5D DD E9 4F  |x.c.........]..O|
0070  00 00 00 00 49 45 4E 44 AE 42 60 82              |....IEND.B`.|
```

Frame boundaries:

| Frame | Offsets | Type | Total bytes |
|---|---|---|---|
| 2 | `0000`-`002A` | RESPONSE | 43 |
| 3 | `002B`-`007B` | DATA | 81 |

### Frame 2: RESPONSE, server to client (43 bytes at offset 0x0000)

| Offset | Bytes | Field | Value |
|---|---|---|---|
| `0000` | `00 00 00 1F` | Length | 31 payload bytes (header not counted) |
| `0004` | `02` | Type | 0x02 = RESPONSE |
| `0005` | `00` | Flags | 0x00 = none |
| `0006` | `00 00` | Reserved | 0 (always zero when sent) |
| `0008` | `00 00 00 01` | Stream ID | 1 |
| `000C` | `00 C8` | Status | 200 |
| `000E` | `03` | Header Count | 3 |
| `000F` | `01` | Header 1: ID | 1 = content-type (name not sent) |
| `0010` | `00 09` | Header 1: Value Length | 9 |
| `0012` | `69 6D 61 67 65 2F 70 6E 67` | Header 1: Value | "image/png" |
| `001B` | `02` | Header 2: ID | 2 = content-length (name not sent) |
| `001C` | `00 02` | Header 2: Value Length | 2 |
| `001E` | `36 39` | Header 2: Value | "69" |
| `0020` | `03` | Header 3: ID | 3 = server (name not sent) |
| `0021` | `00 08` | Header 3: Value Length | 8 |
| `0023` | `62 73 65 72 76 65 2F 31` | Header 3: Value | "bserve/1" |

### Frame 3: DATA, server to client (81 bytes at offset 0x002B)

| Offset | Bytes | Field | Value |
|---|---|---|---|
| `002B` | `00 00 00 45` | Length | 69 payload bytes (header not counted) |
| `002F` | `03` | Type | 0x03 = DATA |
| `0030` | `01` | Flags | 0x01 = END_STREAM |
| `0031` | `00 00` | Reserved | 0 (always zero when sent) |
| `0033` | `00 00 00 01` | Stream ID | 1 |
| `0037` | `89 50 4E 47 0D 0A 1A 0A 00 00 00 0D 49 48 44 52` | Body bytes 0-15 | `.PNG........IHDR` |
| `0047` | `00 00 00 01 00 00 00 01 08 02 00 00 00 90 77 53` | Body bytes 16-31 | `..............wS` |
| `0057` | `DE 00 00 00 0C 49 44 41 54 78 DA 63 B8 D6 A5 0D` | Body bytes 32-47 | `.....IDATx.c....` |
| `0067` | `00 03 C5 01 8C 5D DD E9 4F 00 00 00 00 49 45 4E` | Body bytes 48-63 | `.....]..O....IEN` |
| `0077` | `44 AE 42 60 82` | Body bytes 64-68 | `D.B`.` |

## What was checked

- The DATA payloads add up to 69 bytes and are identical to the file that was served (`www/pixel.png`).
- The client's own `-v` output shows the same 3 frames with exactly the same bytes as the proxy captured.
- The last frame of the response carries END_STREAM, so the client knows the body is complete without any delimiter in the data.
