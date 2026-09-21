import os
import socket
import struct
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import bhttp_server as S

fails = 0


def check(name, cond):
    global fails
    if not cond:
        fails += 1
        print("FAIL", name)


# path rule table
PATHS = [
    (b"/", True), (b"/index.html", True), (b"/css/style.css", True), (b"/docs/", True),
    (b"/a%20b#c", True), (b"/caf\xc3\xa9.txt", True), (b"/.hidden", True),
    (b"index.html", False), (b"", False), (b"/a\x00b", False), (b"/a\x1fb", False),
    (b"/a\x7fb", False), (b"/a\\b", False), (b"/a:b", False), (b"/a*b", False),
    (b"/a?b", False), (b'/a"b', False), (b"/a<b", False), (b"/a>b", False),
    (b"/a|b", False), (b"//a", False), (b"/a//b", False), (b"/./a", False),
    (b"/a/..", False), (b"/..", False), (b"/a./b", False), (b"/a /b", False),
    (b"/a.", False), (b"/con", False), (b"/CON.txt", False), (b"/x/nul.tar.gz", False),
    (b"/com1", False), (b"/LPT9.x", False), (b"/com0", True), (b"/console", True),
    (b"/\xff", False), (b"/\xc3", False),
]
for p, ok in PATHS:
    check(f"path {p!r}", S.valid_path(p) == ok)


def req(method=1, path=b"/x", hdrs=b"", count=0, plen=None, extra=b""):
    return (bytes([method]) + struct.pack(">H", len(path) if plen is None else plen)
            + path + bytes([count]) + hdrs + extra)


def bad(payload, flags=1):
    try:
        S.parse_request(flags, payload)
    except S.Malformed:
        return True
    return False


check("valid min", not bad(req(path=b"/")))
check("method 2", bad(req(method=2)))
check("empty payload", bad(b""))
check("4 byte payload", bad(b"\x01\x00\x01/"))
check("path len 0", bad(b"\x01\x00\x00\x00"))
check("path len over remaining", bad(req(plen=50)))
check("path len 4097", bad(req(path=b"/" + b"a" * 4096)))
check("path len 4096 ok", not bad(req(path=b"/" + b"a" * 4095)))
check("count too high", bad(req(count=1)))
check("header truncated", bad(req(hdrs=b"\x01\x00\x05ab", count=1)))
check("trailing bytes", bad(req(extra=b"\x00")))
check("flags undefined", bad(req(), flags=0x03))
check("no end_stream", bad(req(), flags=0))
check("custom table name", bad(req(hdrs=b"\x00\x00\x06server\x00\x00", count=1)))
check("custom upper", bad(req(hdrs=b"\x00\x00\x01X\x00\x00", count=1)))
check("custom empty name", bad(req(hdrs=b"\x00\x00\x00\x00\x00", count=1)))
check("custom ok", not bad(req(hdrs=b"\x00\x00\x03x-a\x00\x01z", count=1)))
check("unknown id 200 ok", not bad(req(hdrs=b"\xc8\x00\x02hi", count=1)))
check("bad path", bad(req(path=b"/../x")))
check("bad utf8", bad(req(path=b"/\xff")))

# fragmenting reader
frame = S.pack_frame(1, 1, 7, req())
data = frame + S.pack_frame(99, 0xFF, 0, b"zz") + frame
pos = [0]


def one_byte(n):
    if pos[0] >= len(data):
        return b""
    pos[0] += 1
    return data[pos[0] - 1:pos[0]]


got = []
try:
    while True:
        ln, t, f, r, sid = S.read_frame_header(one_byte)
        got.append((t, sid, S.read_exact(one_byte, ln)))
except S.CleanEOF:
    pass
check("1-byte reads yield 3 frames", [g[0] for g in got] == [1, 99, 1] and got[0][1] == 7)
pos[0] = 0
data = frame[:15]
try:
    ln, *_ = S.read_frame_header(one_byte)
    S.read_exact(one_byte, ln)
    check("truncation detected", False)
except S.Truncated:
    pass

# live probe
root = tempfile.mkdtemp()
os.makedirs(os.path.join(root, "docs"))
os.makedirs(os.path.join(root, "empty"))
blob = bytes(range(256)) * 200  # 51200 bytes
exact = os.urandom(32768)
for name, content in [("index.html", b"<h1>hi</h1>\r\n"), ("b.bin", blob), ("exact.bin", exact),
                      ("zero.txt", b""), ("docs/index.html", b"docs"), ("f.txt", b"f")]:
    with open(os.path.join(root, name), "wb") as fh:
        fh.write(content)

proc = subprocess.Popen([sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "bhttp_server.py"),
                         root, "0", "127.0.0.1"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
try:
    line = proc.stdout.readline().decode().strip()
    port = int(line.rsplit(":", 1)[1])

    def connect():
        s = socket.create_connection(("127.0.0.1", port), timeout=5)
        return s

    def rd(s, n):
        b = b""
        while len(b) < n:
            c = s.recv(n - len(b))
            if not c:
                raise EOFError
            b += c
        return b

    def frame_in(s):
        h = rd(s, 12)
        ln, t, f, _, sid = struct.unpack(">IBBHI", h)
        return t, f, sid, rd(s, ln)

    def exchange(s, sid, path, flags=1):
        s.sendall(S.pack_frame(1, flags, sid, req(path=path)))
        t, f, sid2, p = frame_in(s)
        assert t == 2 and sid2 == sid, (t, sid2)
        status = struct.unpack(">H", p[:2])[0]
        body = b""
        frames = []
        while not f & 1:
            t, f, sid3, d = frame_in(s)
            assert t == 3 and sid3 == sid
            frames.append(len(d))
            body += d
        return status, body, frames

    s = connect()
    st, body, fr = exchange(s, 1, b"/")
    check("live /", st == 200 and body == b"<h1>hi</h1>\r\n")
    st, body, fr = exchange(s, 2, b"/b.bin")
    check("live binary", st == 200 and body == blob and fr == [16384, 16384, 16384, 2048])
    st, body, fr = exchange(s, 3, b"/exact.bin")
    check("live exact multiple", st == 200 and body == exact and fr == [16384, 16384])
    st, body, fr = exchange(s, 4, b"/zero.txt")
    check("live empty", st == 200 and body == b"" and fr == [])
    check("live dir", exchange(s, 5, b"/docs")[1] == b"docs" and exchange(s, 6, b"/docs/")[1] == b"docs")
    check("live empty dir 404", exchange(s, 7, b"/empty")[0] == 404)
    check("live missing 404", exchange(s, 8, b"/nope")[0] == 404)
    check("live file trailing slash 404", exchange(s, 9, b"/f.txt/")[0] == 404)
    check("live traversal 400", exchange(s, 10, b"/../x")[0] == 400)
    check("live bad flags 400", exchange(s, 11, b"/", flags=0)[0] == 400)
    # unknown frame skipped, then request works
    s.sendall(S.pack_frame(0x7F, 0xFF, 0, b"junk") + S.pack_frame(1, 1, 12, req(path=b"/f.txt")))
    t, f, sid, p = frame_in(s)
    check("unknown skipped", t == 2 and sid == 12)
    while not f & 1:
        t, f, sid, p = frame_in(s)
    # DATA at server -> 400
    s.sendall(S.pack_frame(3, 1, 13, b"x"))
    t, f, sid, p = frame_in(s)
    check("data->400", t == 2 and sid == 13 and struct.unpack(">H", p[:2])[0] == 400)
    # stream 0 -> ERROR(2)
    s.sendall(S.pack_frame(1, 1, 0, req()))
    t, f, sid, p = frame_in(s)
    check("stream0 error2", t == 4 and sid == 0 and struct.unpack(">H", p[:2])[0] == 2)
    check("closed after error", s.recv(10) == b"")
    s.close()

    s = connect()
    s.sendall(struct.pack(">IBBHI", 16385, 0x55, 0, 0, 0))
    t, f, sid, p = frame_in(s)
    check("oversize unknown -> error1", t == 4 and struct.unpack(">H", p[:2])[0] == 1)
    s.close()
    s = connect()
    s.sendall(b"GET / HTTP/1.1\r\n\r\n")
    t, f, sid, p = frame_in(s)
    check("http text -> error1", t == 4 and struct.unpack(">H", p[:2])[0] == 1)
    s.close()
    s = connect()
    s.sendall(S.pack_frame(4, 0, 0, b"") + S.pack_frame(1, 1, 1, req()))
    try:
        silent = s.recv(10) == b""
    except ConnectionError:
        silent = True
    check("error frame -> silent close", silent)
    s.close()
finally:
    proc.kill()
    proc.wait()

print("selftest", "FAILED" if fails else "OK", f"({fails} failures)")
sys.exit(1 if fails else 0)
