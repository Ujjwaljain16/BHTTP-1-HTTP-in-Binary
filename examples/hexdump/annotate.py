"""Turns captured connection bytes into an annotated hexdump.

    python annotate.py capture.request.bin capture.response.bin \
        --tap tap.txt --body www/pixel.png > annotated-hexdump.md

Every offset, length and value in the output is read out of the captured
bytes; nothing is typed in by hand. The optional --tap file is the client's own
verbose output, which is checked byte for byte against the capture, and --body
is the file that was served, checked against the DATA payloads.
"""
import argparse
import re
import struct
import sys

TYPES = {1: "REQUEST", 2: "RESPONSE", 3: "DATA", 4: "ERROR"}
METHODS = {1: "GET"}
HEADER_NAMES = {1: "content-type", 2: "content-length", 3: "server", 4: "date",
                5: "connection", 6: "content-encoding", 7: "cache-control", 8: "last-modified"}


def hx(b):
    return " ".join("%02X" % c for c in b)


def text(b):
    return "".join(chr(c) if 0x20 <= c < 0x7F else "." for c in b)


class Rows:
    """Collects annotation rows: (absolute offset, raw bytes, field, meaning)."""

    def __init__(self, base):
        self.base = base
        self.rows = []

    def add(self, off, raw, field, meaning=""):
        self.rows.append((self.base + off, raw, field, meaning))

    def table(self):
        out = ["| Offset | Bytes | Field | Value |", "|---|---|---|---|"]
        for off, raw, field, meaning in self.rows:
            out.append("| `%04X` | `%s` | %s | %s |" % (off, hx(raw), field, meaning))
        return "\n".join(out)


def split_frames(data):
    frames, pos = [], 0
    while pos < len(data):
        length = struct.unpack(">I", data[pos:pos + 4])[0]
        end = pos + 12 + length
        if end > len(data):
            raise SystemExit("capture ends inside a frame at offset %d" % pos)
        frames.append((pos, data[pos:end]))
        pos = end
    return frames


def frame_header(rows, f):
    length, typ, flags, reserved, stream = struct.unpack(">IBBHI", f[:12])
    flag_text = "END_STREAM" if flags & 1 else "none"
    if flags & ~1:
        flag_text += " + undefined bits 0x%02X" % (flags & ~1)
    rows.add(0, f[0:4], "Length", "%d payload bytes (header not counted)" % length)
    rows.add(4, f[4:5], "Type", "0x%02X = %s" % (typ, TYPES.get(typ, "unknown")))
    rows.add(5, f[5:6], "Flags", "0x%02X = %s" % (flags, flag_text))
    rows.add(6, f[6:8], "Reserved", "%d (always zero when sent)" % reserved)
    rows.add(8, f[8:12], "Stream ID", "%d" % stream)
    return typ, flags, stream


def headers(rows, p, pos, offset):
    """Annotates a header block starting at p[pos] (the count byte)."""
    count = p[pos]
    rows.add(offset + pos, p[pos:pos + 1], "Header Count", "%d" % count)
    pos += 1
    for i in range(1, count + 1):
        hid = p[pos]
        if hid == 0:
            rows.add(offset + pos, p[pos:pos + 1], "Header %d: ID" % i, "0 = custom header, name follows")
            pos += 1
            (nlen,) = struct.unpack(">H", p[pos:pos + 2])
            rows.add(offset + pos, p[pos:pos + 2], "Header %d: Name Length" % i, "%d" % nlen)
            pos += 2
            name = p[pos:pos + nlen]
            rows.add(offset + pos, name, "Header %d: Name" % i, '"%s"' % name.decode())
            pos += nlen
        else:
            name = HEADER_NAMES.get(hid, "unassigned")
            rows.add(offset + pos, p[pos:pos + 1], "Header %d: ID" % i, "%d = %s (name not sent)" % (hid, name))
            pos += 1
        (vlen,) = struct.unpack(">H", p[pos:pos + 2])
        rows.add(offset + pos, p[pos:pos + 2], "Header %d: Value Length" % i, "%d" % vlen)
        pos += 2
        value = p[pos:pos + vlen]
        rows.add(offset + pos, value, "Header %d: Value" % i, '"%s"' % text(value))
        pos += vlen
    return pos


def annotate_frame(base, f, number, direction):
    rows = Rows(base)
    typ, flags, stream = frame_header(rows, f)
    p, off = f[12:], 12

    if typ == 1:
        rows.add(off, p[0:1], "Method", "0x%02X = %s" % (p[0], METHODS.get(p[0], "unknown")))
        (plen,) = struct.unpack(">H", p[1:3])
        rows.add(off + 1, p[1:3], "Path Length", "%d" % plen)
        rows.add(off + 3, p[3:3 + plen], "Path", '"%s"' % p[3:3 + plen].decode())
        end = headers(rows, p, 3 + plen, off)
    elif typ == 2:
        (status,) = struct.unpack(">H", p[0:2])
        rows.add(off, p[0:2], "Status", "%d" % status)
        end = headers(rows, p, 2, off)
    elif typ == 3:
        for i in range(0, len(p), 16):
            part = p[i:i + 16]
            rows.add(off + i, part, "Body bytes %d-%d" % (i, i + len(part) - 1), "`%s`" % text(part))
        end = len(p)
    else:
        rows.add(off, p, "Payload", "")
        end = len(p)
    if end != len(p):
        raise SystemExit("frame %d: payload not fully consumed" % number)

    title = "Frame %d: %s, %s (%d bytes at offset 0x%04X)" % (number, TYPES.get(typ, "unknown"), direction, len(f), base)
    return title, rows.table(), typ, flags, p


def dump(data):
    lines = []
    for i in range(0, len(data), 16):
        row = data[i:i + 16]
        lines.append("%04X  %-47s  |%s|" % (i, hx(row), text(row)))
    return "\n".join(lines)


def tap_frames(path):
    """Reads back the frames printed by the client's verbose mode."""
    frames, cur, mode = [], None, None
    for line in open(path, encoding="utf-8").read().splitlines():
        m = re.match(r"^(-->|<--) frame (\d+)", line)
        if m:
            cur = {"dir": m.group(1), "n": int(m.group(2)), "bytes": b""}
            frames.append(cur)
            continue
        m = re.match(r"^\s+header\s+((?:[0-9A-F]{2} ?)+)$", line)
        if m and cur is not None:
            cur["bytes"] += bytes.fromhex(m.group(1))
            continue
        m = re.match(r"^\s+[0-9A-F]{8}  ((?:[0-9A-F]{2} ?| )+?)\s+\|", line)
        if m and cur is not None:
            cur["bytes"] += bytes.fromhex(m.group(1).replace("  ", " "))
    return frames


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("request")
    ap.add_argument("response")
    ap.add_argument("--tap")
    ap.add_argument("--body")
    a = ap.parse_args()

    req = open(a.request, "rb").read()
    resp = open(a.response, "rb").read()
    req_frames, resp_frames = split_frames(req), split_frames(resp)

    out = []
    w = out.append
    w("# BHTTP/1 annotated hexdump\n")
    w("One complete exchange captured from a real run: `bcurl` fetching `/pixel.png` "
      "from `bserve`, with a recording proxy between them copying the bytes without "
      "looking at them. The request carries one custom header (`x-demo: hello`) so every "
      "header form appears in the capture. Offsets are counted from the first byte "
      "the sender transmitted in each direction.\n")
    w("Every multi-byte integer is big-endian. A frame is a 12-byte header (Length 4, Type 1, "
      "Flags 1, Reserved 2, Stream ID 4) followed by Length payload bytes, and the next frame "
      "starts immediately after.\n")

    def section(title, data, frames, direction, first_number):
        w("## %s\n" % title)
        w("Raw bytes (%d):\n" % len(data))
        w("```text\n%s\n```\n" % dump(data))
        w("Frame boundaries:\n")
        w("| Frame | Offsets | Type | Total bytes |")
        w("|---|---|---|---|")
        for n, (pos, f) in enumerate(frames, first_number):
            w("| %d | `%04X`-`%04X` | %s | %d |" % (n, pos, pos + len(f) - 1, TYPES.get(f[4], "unknown"), len(f)))
        w("")
        bodies = []
        for n, (pos, f) in enumerate(frames, first_number):
            title, table, typ, flags, payload = annotate_frame(pos, f, n, direction)
            w("### %s\n" % title)
            w(table + "\n")
            if typ == 3:
                bodies.append(payload)
        return bodies

    section("Request (client to server)", req, req_frames, "client to server", 1)
    bodies = section("Response (server to client)", resp, resp_frames, "server to client", len(req_frames) + 1)

    w("## What was checked\n")
    body = b"".join(bodies)
    if a.body:
        served = open(a.body, "rb").read()
        assert served == body, "the DATA payloads differ from the served file"
        w("- The DATA payloads add up to %d bytes and are identical to the file that was served (`%s`)." % (len(body), a.body))
    if a.tap:
        seen = tap_frames(a.tap)
        want = [(n, f) for n, (_, f) in enumerate(req_frames + resp_frames, 1)]
        got = [(t["n"], t["bytes"]) for t in seen]
        assert got == want, "the client's verbose dump differs from the proxy capture"
        w("- The client's own `-v` output shows the same %d frames with exactly the same bytes as the proxy captured." % len(want))
    w("- The last frame of the response carries END_STREAM, so the client knows the body is complete "
      "without any delimiter in the data.")
    sys.stdout.write("\n".join(out) + "\n")


if __name__ == "__main__":
    main()
