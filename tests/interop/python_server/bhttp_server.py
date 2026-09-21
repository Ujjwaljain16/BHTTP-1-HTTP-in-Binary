#!/usr/bin/env python3
"""BHTTP/1 file server, independent implementation. Usage: bhttp_server.py ROOT PORT [HOST]"""
import os
import socket
import struct
import sys
import threading
import time

MAX_PAYLOAD = 16384
MAX_PATH = 4096
T_REQUEST, T_RESPONSE, T_DATA, T_ERROR = 1, 2, 3, 4
END_STREAM = 0x01
ERR_TOO_LARGE, ERR_STREAM_ID, ERR_PROTOCOL = 1, 2, 3
IDLE_TIMEOUT = 30.0
DRAIN_SECONDS = 1.0
DRAIN_BYTES = 64 * 1024

TABLE_NAMES = {b"content-type", b"content-length", b"server", b"date",
               b"connection", b"content-encoding", b"cache-control", b"last-modified"}
H_CONTENT_TYPE, H_CONTENT_LENGTH, H_SERVER = 1, 2, 3
SERVER_NAME = b"bhttp-py/1"

RESERVED_NAMES = {"CON", "PRN", "AUX", "NUL"} | {f"COM{i}" for i in range(1, 10)} | {f"LPT{i}" for i in range(1, 10)}
FORBIDDEN_CHARS = set(b'\\:*?"<>|')
CUSTOM_NAME_BYTES = set(b"abcdefghijklmnopqrstuvwxyz0123456789-_.")

MIME = {
    ".html": b"text/html", ".htm": b"text/html", ".css": b"text/css",
    ".js": b"text/javascript", ".json": b"application/json", ".txt": b"text/plain",
    ".png": b"image/png", ".jpg": b"image/jpeg", ".jpeg": b"image/jpeg",
    ".gif": b"image/gif", ".svg": b"image/svg+xml", ".pdf": b"application/pdf",
}


class Truncated(Exception):
    pass


class CleanEOF(Exception):
    pass


class Malformed(Exception):
    pass


class ConnFault(Exception):
    def __init__(self, code, msg):
        super().__init__(msg)
        self.code = code
        self.msg = msg


# ---------------------------------------------------------------- codec

def pack_frame(ftype, flags, stream, payload=b""):
    return struct.pack(">IBBHI", len(payload), ftype, flags, 0, stream) + payload


def read_exact(recv, n):
    """recv is a callable(max_bytes)->bytes. Returns exactly n bytes or raises Truncated."""
    buf = bytearray()
    while len(buf) < n:
        chunk = recv(n - len(buf))
        if not chunk:
            raise Truncated()
        buf += chunk
    return bytes(buf)


def read_frame_header(recv):
    """Returns (length, type, flags, reserved, stream) or raises CleanEOF / Truncated."""
    buf = bytearray()
    while len(buf) < 12:
        chunk = recv(12 - len(buf))
        if not chunk:
            if not buf:
                raise CleanEOF()
            raise Truncated()
        buf += chunk
    return struct.unpack(">IBBHI", bytes(buf))


def valid_path(raw):
    """Path rules for a file server; raw is bytes."""
    if not raw or raw[0:1] != b"/":
        return False
    for b in raw:
        if b < 0x20 or b == 0x7F or b in FORBIDDEN_CHARS:
            return False
    if b"//" in raw:
        return False
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        return False
    for seg in text.split("/")[1:]:
        if seg == "":
            continue
        if seg in (".", ".."):
            return False
        if seg.endswith(".") or seg.endswith(" "):
            return False
        stem = seg.split(".", 1)[0].upper()
        if stem in RESERVED_NAMES:
            return False
    return True


def parse_headers(payload, pos, count):
    """Returns (list of (id, name_or_None, value), new_pos). Raises Malformed."""
    out = []
    n = len(payload)
    for _ in range(count):
        if pos + 1 > n:
            raise Malformed("header id past end")
        hid = payload[pos]
        pos += 1
        name = None
        if hid == 0:
            if pos + 2 > n:
                raise Malformed("name length past end")
            (nl,) = struct.unpack_from(">H", payload, pos)
            pos += 2
            if nl < 1 or nl > 255 or pos + nl > n:
                raise Malformed("bad name length")
            name = payload[pos:pos + nl]
            pos += nl
            if any(b not in CUSTOM_NAME_BYTES for b in name):
                raise Malformed("bad custom name")
            if name in TABLE_NAMES:
                raise Malformed("custom header uses table name")
        if pos + 2 > n:
            raise Malformed("value length past end")
        (vl,) = struct.unpack_from(">H", payload, pos)
        pos += 2
        if pos + vl > n:
            raise Malformed("value past end")
        out.append((hid, name, payload[pos:pos + vl]))
        pos += vl
    return out, pos


def parse_request(flags, payload):
    """Returns (path_bytes, headers). Raises Malformed for anything that deserves 400."""
    if flags & 0xFE:
        raise Malformed("undefined flag bits")
    if not flags & END_STREAM:
        raise Malformed("no END_STREAM")
    if len(payload) < 5:
        raise Malformed("short payload")
    if payload[0] != 0x01:
        raise Malformed("unknown method")
    (pl,) = struct.unpack_from(">H", payload, 1)
    if pl == 0 or pl > MAX_PATH or 3 + pl + 1 > len(payload):
        raise Malformed("bad path length")
    path = payload[3:3 + pl]
    count = payload[3 + pl]
    headers, end = parse_headers(payload, 4 + pl, count)
    if end != len(payload):
        raise Malformed("trailing bytes")
    if not valid_path(path):
        raise Malformed("invalid path")
    return path, headers


def build_header(hid, value):
    return struct.pack(">BH", hid, len(value)) + value


def build_response_payload(status, headers):
    body = struct.pack(">HB", status, len(headers))
    for hid, value in headers:
        body += build_header(hid, value)
    return body


def build_error_payload(code, msg):
    m = msg.encode("utf-8")
    return struct.pack(">HH", code, len(m)) + m


# ---------------------------------------------------------------- file mapping

def _inside(root_real, candidate_real):
    a = os.path.normcase(root_real)
    b = os.path.normcase(candidate_real)
    try:
        return os.path.commonpath([a, b]) == a
    except ValueError:
        return False


def resolve(root_real, raw_path):
    """Returns real file path, or None for 404. raw_path already passed valid_path."""
    text = raw_path.decode("utf-8")
    trailing = text.endswith("/")
    rel = text[1:]
    parts = [p for p in rel.split("/") if p != ""]
    candidate = os.path.join(root_real, *parts) if parts else root_real
    try:
        real = os.path.realpath(candidate)
        if not _inside(root_real, real):
            return None
        if os.path.isdir(real):
            idx = os.path.realpath(os.path.join(real, "index.html"))
            if not _inside(root_real, idx) or not os.path.isfile(idx):
                return None
            return idx
        if trailing:
            return None
        if os.path.isfile(real):
            return real
    except (OSError, ValueError):
        return None
    return None


def content_type_for(path):
    ext = os.path.splitext(path)[1].lower()
    return MIME.get(ext, b"application/octet-stream")


# ---------------------------------------------------------------- connection

def log(msg):
    sys.stderr.write(msg + "\n")
    sys.stderr.flush()


class Conn:
    def __init__(self, sock, addr, root_real):
        self.sock = sock
        self.addr = addr
        self.root = root_real

    def recv(self, n):
        return self.sock.recv(min(n, 65536))

    def send(self, data):
        self.sock.sendall(data)

    def simple_response(self, stream, status):
        hdrs = [(H_CONTENT_LENGTH, b"0"), (H_SERVER, SERVER_NAME)]
        self.send(pack_frame(T_RESPONSE, END_STREAM, stream, build_response_payload(status, hdrs)))

    def fault(self, code, msg):
        try:
            self.send(pack_frame(T_ERROR, 0, 0, build_error_payload(code, msg)))
            self.sock.shutdown(socket.SHUT_WR)
        except OSError:
            return
        deadline = time.monotonic() + DRAIN_SECONDS
        drained = 0
        while drained < DRAIN_BYTES:
            left = deadline - time.monotonic()
            if left <= 0:
                break
            self.sock.settimeout(left)
            try:
                chunk = self.sock.recv(4096)
            except (OSError, socket.timeout):
                break
            if not chunk:
                break
            drained += len(chunk)

    def serve_file(self, stream, path):
        try:
            f = open(path, "rb")
        except OSError:
            self.simple_response(stream, 500)
            return True
        with f:
            try:
                size = os.fstat(f.fileno()).st_size
            except OSError:
                self.simple_response(stream, 500)
                return True
            hdrs = [(H_CONTENT_TYPE, content_type_for(path)),
                    (H_CONTENT_LENGTH, str(size).encode("ascii")),
                    (H_SERVER, SERVER_NAME)]
            if size == 0:
                self.send(pack_frame(T_RESPONSE, END_STREAM, stream, build_response_payload(200, hdrs)))
                return True
            self.send(pack_frame(T_RESPONSE, 0, stream, build_response_payload(200, hdrs)))
            sent = 0
            pending = None
            try:
                while sent < size:
                    chunk = f.read(min(MAX_PAYLOAD, size - sent))
                    if not chunk:
                        raise OSError("file shrank")
                    sent += len(chunk)
                    if pending is not None:
                        self.send(pack_frame(T_DATA, 0, stream, pending))
                    pending = chunk
                self.send(pack_frame(T_DATA, END_STREAM, stream, pending))
            except OSError as e:
                if isinstance(e, (ConnectionError, socket.timeout)):
                    raise
                self.fault(ERR_PROTOCOL, "internal error")
                return False
        return True

    def handle_request(self, stream, flags, payload):
        try:
            path, _ = parse_request(flags, payload)
        except Malformed as e:
            log(f"{self.addr} stream {stream}: 400 ({e})")
            self.simple_response(stream, 400)
            return
        real = resolve(self.root, path)
        if real is None:
            log(f"{self.addr} stream {stream}: 404 {path!r}")
            self.simple_response(stream, 404)
            return
        log(f"{self.addr} stream {stream}: GET {path!r}")
        return self.serve_file(stream, real)

    def run(self):
        self.sock.settimeout(IDLE_TIMEOUT)
        try:
            while True:
                length, ftype, flags, _res, stream = read_frame_header(self.recv)
                if length > MAX_PAYLOAD:
                    if ftype == T_ERROR:
                        return
                    self.fault(ERR_TOO_LARGE, "frame too large")
                    return
                if ftype not in (T_REQUEST, T_RESPONSE, T_DATA, T_ERROR):
                    read_exact(self.recv, length)
                    continue
                payload = read_exact(self.recv, length)
                if ftype in (T_REQUEST, T_RESPONSE, T_DATA) and stream == 0:
                    self.fault(ERR_STREAM_ID, "stream id 0 is not valid here")
                    return
                if ftype == T_ERROR:
                    return
                if ftype in (T_RESPONSE, T_DATA):
                    self.simple_response(stream, 400)
                    continue
                if self.handle_request(stream, flags, payload) is False:
                    return
        except (CleanEOF, Truncated, socket.timeout, OSError):
            pass
        finally:
            try:
                self.sock.close()
            except OSError:
                pass


def main():
    if len(sys.argv) < 3:
        sys.stderr.write("usage: bhttp_server.py ROOT PORT [HOST]\n")
        return 2
    root = os.path.realpath(sys.argv[1])
    if not os.path.isdir(root):
        sys.stderr.write(f"not a directory: {sys.argv[1]}\n")
        return 2
    port = int(sys.argv[2])
    host = sys.argv[3] if len(sys.argv) > 3 else "0.0.0.0"
    ls = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    ls.bind((host, port))
    ls.listen(64)
    bound = ls.getsockname()
    print(f"listening on {bound[0]}:{bound[1]}", flush=True)
    while True:
        try:
            s, addr = ls.accept()
        except OSError:
            break
        threading.Thread(target=Conn(s, addr, root).run, daemon=True).start()
    return 0


if __name__ == "__main__":
    sys.exit(main())
