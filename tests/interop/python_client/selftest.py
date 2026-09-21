"""Offline checks; expected bytes are derived by hand from the spec text."""
import struct
import sys

import bhttp_client as b


class FakeSock:
    """recv returns at most `step` bytes; sendall records."""

    def __init__(self, data=b"", step=1):
        self.data, self.pos, self.step, self.out = data, 0, step, b""
        self.shut = False
        self.closed = False

    def recv(self, n):
        k = min(n, self.step, len(self.data) - self.pos)
        r = self.data[self.pos:self.pos + k]
        self.pos += k
        return r

    def sendall(self, d):
        self.out += d

    def shutdown(self, how):
        self.shut = True

    def close(self):
        self.closed = True


def raises(exc, fn):
    try:
        fn()
    except exc:
        return True
    except Exception as e:
        print("wrong exception", type(e), e)
        return False
    return False


def test_frame_header():
    # Length=5 Type=1 Flags=1 Reserved=0 Stream=1 -> 00000005 01 01 0000 00000001
    assert b.pack_frame(1, 1, 1, b"abcde") == bytes.fromhex("00000005 01 01 0000 00000001".replace(" ", "")) + b"abcde"
    assert b.pack_frame(3, 0, 0x01020304, b"") == bytes.fromhex("000000000300000001020304".replace(" ", "")[:0] or "00000000 03 00 0000 01020304".replace(" ", ""))


def test_ints():
    assert struct.pack(">H", 200) == b"\x00\xC8"
    assert b.encode_response(200) == bytes.fromhex("00C8 00".replace(" ", ""))
    assert b.encode_response(404) == bytes.fromhex("0194" "00")
    assert b.encode_response(500) == bytes.fromhex("01F4" "00")
    assert b.encode_response(400)[:2] == bytes.fromhex("0190")


def test_request_encoding():
    # Method 01, PathLen 000B, "/index.html", HeaderCount 00
    exp = bytes.fromhex("01" "000B") + b"/index.html" + b"\x00"
    assert b.encode_request("/index.html") == exp
    # with known header content-type(1)="a" and custom "x-a"="v"
    p = b.encode_request("/", [("content-type", "a"), ("x-a", "v")])
    assert p == bytes.fromhex("01" "0001") + b"/" + b"\x02" + bytes.fromhex("01 0001".replace(" ", "")) + b"a" \
        + bytes.fromhex("00 0003".replace(" ", "")) + b"x-a" + bytes.fromhex("0001") + b"v"
    # table name never goes custom
    assert b.encode_header("server", "s") == b"\x03\x00\x01s"
    assert raises(ValueError, lambda: b.encode_header("X-Upper", "v"))
    assert raises(ValueError, lambda: b.encode_header("a b", "v"))
    assert raises(ValueError, lambda: b.encode_request("", []))
    assert raises(ValueError, lambda: b.encode_request("/" + "a" * 4096, []))
    assert len(b.encode_request("/" + "a" * 4095)) == 3 + 4096 + 1


def test_response_decoding():
    payload = bytes.fromhex("00C8 03".replace(" ", "")) \
        + b"\x01\x00\x09text/html" + b"\x02\x00\x02" + b"12" + b"\x00\x00\x03x-a\x00\x01v"
    payload = payload[:2] + b"\x03" + payload[3:]
    st, hs = b.decode_response(payload)
    assert st == 200
    assert hs == [(1, "content-type", b"text/html"), (2, "content-length", b"12"), (0, "x-a", b"v")]
    # unknown ID 200 parsed with known layout and kept, name None
    st, hs = b.decode_response(b"\x00\xC8\x02" + b"\xC8\x00\x02hi" + b"\x03\x00\x00")
    assert hs == [(200, None, b"hi"), (3, "server", b"")]
    # trailing byte / overrun / short
    assert raises(b.Malformed, lambda: b.decode_response(b"\x00\xC8\x00\x00"))
    assert raises(b.Malformed, lambda: b.decode_response(b"\x00\xC8\x01\x01\x00\x05ab"))
    assert raises(b.Malformed, lambda: b.decode_response(b"\x00\xC8\x02\x03\x00\x00"))
    assert raises(b.Malformed, lambda: b.decode_response(b"\x00\xC8"))


def test_error_codec():
    assert b.encode_error(1, "hi") == bytes.fromhex("0001 0002".replace(" ", "")) + b"hi"
    assert b.decode_error(b.encode_error(2, "x")) == (2, b"x")
    assert b.decode_error(b"\x00")[0] is None
    assert b.decode_error(b"\x00\x03\x00\x05ab")[0] is None
    assert b.decode_error(b.encode_error(9, b"")) == (9, b"")


def test_reader_chunked():
    f1 = b.pack_frame(2, 0, 1, b.encode_response(200, [("content-length", "3")]))
    unk = b.pack_frame(0x05, 0xFF, 0, b"zzzz", reserved=0xABCD)
    f2 = b.pack_frame(3, 1, 1, b"\x00\xffA")
    for step in (1, 2, 5, 7, 4096):
        s = FakeSock(unk + f1 + f2, step)
        a = b.read_frame(s)
        assert a.type == 2 and a.stream_id == 1 and a.flags == 0, a   # unknown skipped
        d = b.read_frame(s)
        assert d.type == 3 and d.payload == b"\x00\xffA" and d.end_stream
        assert b.read_frame(s) is None                                   # clean EOF
    # skip_unknown=False exposes it
    s = FakeSock(unk, 1)
    assert b.read_frame(s, skip_unknown=False).type == 5
    # truncation
    assert raises(b.Truncated, lambda: b.read_frame(FakeSock(f2[:5], 1)))
    assert raises(b.Truncated, lambda: b.read_frame(FakeSock(f2[:-1], 1)))
    assert raises(b.Truncated, lambda: b.read_frame(FakeSock(unk[:-1], 3)))
    # oversize: only header needed, nothing allocated
    e = None
    try:
        b.read_frame(FakeSock(struct.pack(">IBBHI", 16385, 3, 0, 0, 1), 1))
    except b.FrameFault as ex:
        e = ex
    assert e and e.code == 1
    # oversize even for unknown type
    assert raises(b.FrameFault, lambda: b.read_frame(FakeSock(struct.pack(">IBBHI", 16385, 0x77, 0, 0, 0), 1)))
    # exactly 16384 OK
    assert len(b.read_frame(FakeSock(b.pack_frame(3, 1, 1, b"x" * 16384), 1000)).payload) == 16384
    # stream 0 on known types 1-3 -> fault 2; ERROR with stream != 0 fine; unknown with stream 0 fine
    for t in (1, 2, 3):
        e = None
        try:
            b.read_frame(FakeSock(b.pack_frame(t, 1, 0, b"x"), 1))
        except b.FrameFault as ex:
            e = ex
        assert e and e.code == 2
    assert b.read_frame(FakeSock(b.pack_frame(4, 0, 7, b.encode_error(3)), 1)).type == 4


def test_client_exchange():
    body = bytes(range(256)) * 3
    resp = b.pack_frame(2, 0, 1, b.encode_response(
        200, [("content-type", "application/octet-stream"), ("content-length", str(len(body))), ("server", "t")]))
    d1 = b.pack_frame(3, 0, 1, body[:100])
    d2 = b.pack_frame(3, 1, 1, body[100:])
    empty = b.pack_frame(2, 1, 2, b.encode_response(404, [("content-length", "0"), ("server", "t")]))
    s = FakeSock(b.pack_frame(0x09, 0, 0, b"?") + resp + d1 + d2 + empty, 1)
    c = b.Client(None, None, sock=s)
    r = c.get("/x")
    assert r.status == 200 and r.body == body and r.stream_id == 1
    assert r.header_ids() == [1, 2, 3]
    assert s.out == b.pack_frame(1, 1, 1, b.encode_request("/x"))
    r = c.get("/y")
    assert r.status == 404 and r.body == b"" and r.stream_id == 2
    assert s.out.endswith(b.pack_frame(1, 1, 2, b.encode_request("/y")))
    assert c.next_stream == 3


def _one(frames, sid=1):
    c = b.Client(None, None, sock=FakeSock(frames, 3))
    return c, sid


def test_client_failures():
    ok = b.encode_response(200, [("content-length", "1")])
    cases = {
        "wrong stream": (b.pack_frame(2, 1, 9, b.encode_response(404)), b.ProtocolFailure),
        "DATA first": (b.pack_frame(3, 1, 1, b"x"), b.ProtocolFailure),
        "ERROR": (b.pack_frame(4, 0, 0, b.encode_error(3, "x")), b.PeerError),
        "EOF": (b"", b.IncompleteResponse),
        "EOF after response": (b.pack_frame(2, 0, 1, ok), b.IncompleteResponse),
        "truncated": (b.pack_frame(2, 0, 1, ok)[:-1], b.Truncated),
        "bad status": (b.pack_frame(2, 1, 1, b"\x00\x63\x00"), b.ProtocolFailure),
        "bad status 600": (b.pack_frame(2, 1, 1, b"\x02\x58\x00"), b.ProtocolFailure),
        "trailing": (b.pack_frame(2, 1, 1, b"\x00\xC8\x00\x00"), b.ProtocolFailure),
        "REQUEST at client": (b.pack_frame(1, 1, 1, b.encode_request("/")), b.ProtocolFailure),
        "repeat cl, one bad": (b.pack_frame(2, 1, 1, b.encode_response(
            200, [("content-length", "0"), ("content-length", "1")])), b.ProtocolFailure),
        "name length 0": (b.pack_frame(2, 1, 1, bytes.fromhex("00C8 01 00 0000 00 0000".replace(" ", ""))),
                          b.ProtocolFailure),
        "header overruns": (b.pack_frame(2, 1, 1, bytes.fromhex("00C8 01 01 0009") + b"ab"), b.ProtocolFailure),
        "cl mismatch": (b.pack_frame(2, 0, 1, ok) + b.pack_frame(3, 1, 1, b"xx"), b.ProtocolFailure),
        "cl nondigit": (b.pack_frame(2, 1, 1, b.encode_response(200, [("content-length", "+0")])), b.ProtocolFailure),
        "oversize": (struct.pack(">IBBHI", 16385, 3, 0, 0, 1), b.FrameFault),
        "stream0": (b.pack_frame(2, 1, 0, b"\x00\xC8\x00"), b.FrameFault),
        "second RESPONSE": (b.pack_frame(2, 0, 1, ok) + b.pack_frame(2, 1, 1, ok), b.ProtocolFailure),
    }
    for name, (data, exc) in cases.items():
        c, sid = _one(data)
        assert raises(exc, lambda: c.read_response(sid)), name
    # every failure closes the connection and marks it broken (SPEC 12.8)
    c, _ = _one(cases["cl mismatch"][0])
    raises(b.ProtocolFailure, lambda: c.read_response(1))
    assert c.broken and c.sock.closed
    # REQUEST at client -> ERROR(3) + close
    s = FakeSock(b.pack_frame(1, 1, 1, b.encode_request("/")), 1)
    c = b.Client(None, None, sock=s)
    raises(b.ProtocolFailure, lambda: c.read_response(1))
    assert s.out == b.pack_frame(4, 0, 0, b.encode_error(3, b"protocol error")) and s.shut and s.closed
    # oversize ERROR frame: never answered (SPEC 4.2, 9.1), still a failure
    s = FakeSock(struct.pack(">IBBHI", 16385, 4, 0, 0, 0), 1)
    c = b.Client(None, None, sock=s)
    assert raises(b.FrameFault, lambda: c.read_response(1))
    assert s.out == b"" and s.closed
    # oversize -> client sends ERROR(1) and half-closes
    s = FakeSock(struct.pack(">IBBHI", 16385, 3, 0, 0, 1), 1)
    c = b.Client(None, None, sock=s)
    raises(b.FrameFault, lambda: c.read_response(1))
    assert s.out == b.pack_frame(4, 0, 0, b.encode_error(1, b"protocol error")) and s.shut
    # client never replies to a received ERROR
    s = FakeSock(b.pack_frame(4, 0, 0, b.encode_error(3, "x")), 1)
    c = b.Client(None, None, sock=s)
    raises(b.PeerError, lambda: c.read_response(1))
    assert s.out == b""
    # broken connection refuses further requests
    assert raises(b.BhttpError, lambda: c.get("/z"))
    # 1xx/3xx are final, non-error responses (SPEC 12.6)
    for st in (100, 204, 301, 599):
        c, _ = _one(b.pack_frame(2, 1, 1, b.encode_response(st)))
        assert c.read_response(1).status == st
    # undefined flag bits on RESPONSE/DATA are ignored (SPEC 3); END_STREAM still honoured
    c, _ = _one(b.pack_frame(2, 0xFE, 1, b.encode_response(200)) + b.pack_frame(3, 0x81, 1, b"ab"))
    assert c.read_response(1).body == b"ab"
    # ERROR with junk flags/stream/short payload: connection failure, no reply
    for pl in (b"", b"\x00", bytes.fromhex("0009 0003 fffefd"), bytes.fromhex("0001 0000") + b"zz"):
        s = FakeSock(b.pack_frame(4, 0xFF, 7, pl), 2)
        c = b.Client(None, None, sock=s)
        assert raises(b.PeerError, lambda: c.read_response(1)) and s.out == b""
    # repeated content-length, all equal: fine
    c, _ = _one(b.pack_frame(2, 0, 1, b.encode_response(200, [("content-length", "2"), ("content-length", "2")]))
                + b.pack_frame(3, 1, 1, b"ab"))
    assert c.read_response(1).body == b"ab"
    # stray frame after END_STREAM is caught lazily on the next exchange (SPEC 12.3)
    c, _ = _one(b.pack_frame(2, 1, 1, b.encode_response(404)) + b.pack_frame(3, 1, 1, b"x"))
    assert c.read_response(1).status == 404
    assert raises(b.ProtocolFailure, lambda: c.read_response(2))
    # IDs consumed by every request incl. malformed ones; no wraparound (SPEC 5.2)
    s = FakeSock(b"", 1)
    c = b.Client(None, None, sock=s)
    c.send_request("/a", method=0x02)
    assert c.next_stream == 2
    c.next_stream = 0xFFFFFFFF
    assert c.send_request("/a") == 0xFFFFFFFF
    assert raises(b.BhttpError, lambda: c.send_request("/a")) and c.broken and s.closed
    # empty DATA with END_STREAM tolerated
    c, _ = _one(b.pack_frame(2, 0, 1, b.encode_response(200)) + b.pack_frame(3, 1, 1, b""))
    assert c.read_response(1).body == b""


def test_hexdump():
    assert b.hexdump(b"AB").startswith("00000000  41 42")


def main():
    n = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn()
            n += 1
            print("ok  ", name)
    print("%d test groups passed" % n)


if __name__ == "__main__":
    sys.exit(main())
