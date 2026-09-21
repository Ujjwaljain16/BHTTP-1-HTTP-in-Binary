"""BHTTP/1 client, written only from SPEC.md / WIRE_FORMAT.md / HEADER_TABLE.md.

Python 3.11 standard library only.  No code shared with any other implementation.
"""
import socket
import struct

MAX_PAYLOAD = 16384
MAX_PATH = 4096
FRAME_HDR = struct.Struct(">IBBHI")  # Length u32, Type u8, Flags u8, Reserved u16, Stream u32
assert FRAME_HDR.size == 12

T_REQUEST, T_RESPONSE, T_DATA, T_ERROR = 1, 2, 3, 4
KNOWN_TYPES = (T_REQUEST, T_RESPONSE, T_DATA, T_ERROR)
END_STREAM = 0x01
METHOD_GET = 0x01

E_FRAME_TOO_LARGE, E_INVALID_STREAM_ID, E_PROTOCOL_ERROR = 1, 2, 3

HEADER_TABLE = {
    1: "content-type", 2: "content-length", 3: "server", 4: "date",
    5: "connection", 6: "content-encoding", 7: "cache-control", 8: "last-modified",
}
HEADER_IDS = {v: k for k, v in HEADER_TABLE.items()}
_CUSTOM_OK = set(b"abcdefghijklmnopqrstuvwxyz0123456789-_.")


# ---------------------------------------------------------------- exceptions
class BhttpError(Exception):
    pass


class Truncated(BhttpError):
    """EOF inside a frame header or payload."""


class FrameFault(BhttpError):
    """Connection-level fault detected on receive (code 1/2/3)."""

    def __init__(self, code, msg, reply=True):
        super().__init__(msg)
        self.code = code
        self.reply = reply   # False: ERROR frame with oversize Length (SPEC 4.2): just close


class Malformed(BhttpError):
    """Payload of a known frame does not decode."""


class ProtocolFailure(BhttpError):
    """Peer violated SPEC section 12 client rules."""


class PeerError(BhttpError):
    """ERROR frame received (connection failed)."""

    def __init__(self, code, msg):
        super().__init__("peer ERROR code=%r msg=%r" % (code, msg))
        self.code = code
        self.msg = msg


class IncompleteResponse(BhttpError):
    """EOF before END_STREAM."""


# ------------------------------------------------------------------ frames
class Frame:
    __slots__ = ("length", "type", "flags", "reserved", "stream_id", "payload")

    def __init__(self, length, type_, flags, reserved, stream_id, payload):
        self.length, self.type, self.flags = length, type_, flags
        self.reserved, self.stream_id, self.payload = reserved, stream_id, payload

    @property
    def end_stream(self):
        return bool(self.flags & END_STREAM)

    def __repr__(self):
        return "Frame(type=0x%02x flags=0x%02x stream=%d len=%d)" % (
            self.type, self.flags, self.stream_id, self.length)


def pack_frame(type_, flags, stream_id, payload=b"", length=None, reserved=0):
    """Serialize a frame. `length` overrides the Length field (for crafted tests)."""
    if length is None:
        length = len(payload)
    return FRAME_HDR.pack(length, type_, flags, reserved, stream_id) + bytes(payload)


def read_exact(sock, n):
    """Read exactly n bytes, looping on recv. Returns b'' only if n==0.
    Raises Truncated if EOF after >=1 byte; raises EOFError if EOF at 0 bytes."""
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            if not buf:
                raise EOFError()
            raise Truncated("EOF after %d of %d bytes" % (len(buf), n))
        buf += chunk
    return bytes(buf)


def _discard(sock, n):
    while n > 0:
        chunk = sock.recv(min(n, 4096))
        if not chunk:
            raise Truncated("EOF while skipping unknown frame")
        n -= len(chunk)


def read_frame(sock, skip_unknown=True):
    """Read the next frame per WIRE_FORMAT section 10.

    Returns None on clean EOF at a frame boundary.
    Raises FrameFault(1) for Length>16384, FrameFault(2) for stream 0 on types 1-3,
    Truncated on EOF inside a frame.  Unknown types are skipped unless
    skip_unknown=False (then returned with empty payload discarded? no: payload is
    read and returned so tests can observe them; still bounded by 16384)."""
    while True:
        try:
            hdr = read_exact(sock, 12)
        except EOFError:
            return None
        length, type_, flags, reserved, sid = FRAME_HDR.unpack(hdr)
        if length > MAX_PAYLOAD:
            # SPEC 4.2 / WIRE 10: an oversize ERROR is never answered, just closed.
            raise FrameFault(E_FRAME_TOO_LARGE, "Length %d > %d" % (length, MAX_PAYLOAD),
                             reply=(type_ != T_ERROR))
        if type_ not in KNOWN_TYPES:
            if skip_unknown:
                _discard(sock, length)
                continue
            payload = read_exact(sock, length) if length else b""
            return Frame(length, type_, flags, reserved, sid, payload)
        try:
            payload = read_exact(sock, length) if length else b""
        except EOFError:
            raise Truncated("EOF before payload")
        if type_ in (T_REQUEST, T_RESPONSE, T_DATA) and sid == 0:
            raise FrameFault(E_INVALID_STREAM_ID, "stream id 0 on type %d" % type_)
        return Frame(length, type_, flags, reserved, sid, payload)


def write_all(sock, data):
    sock.sendall(data)


# ----------------------------------------------------------------- headers
def valid_custom_name(name: bytes):
    return 1 <= len(name) <= 255 and all(b in _CUSTOM_OK for b in name) \
        and name.decode("ascii") not in HEADER_IDS


def encode_header(name, value):
    """name: str/bytes (table names use their ID) or int (raw known ID)."""
    if isinstance(value, str):
        value = value.encode("utf-8")
    if len(value) > 65535:
        raise ValueError("value too long")
    if isinstance(name, int):
        if not 1 <= name <= 255:
            raise ValueError("bad id")
        return struct.pack(">BH", name, len(value)) + value
    nb = name.encode("ascii") if isinstance(name, str) else bytes(name)
    if nb.decode("ascii") in HEADER_IDS:
        return struct.pack(">BH", HEADER_IDS[nb.decode("ascii")], len(value)) + value
    if not valid_custom_name(nb):
        raise ValueError("invalid custom header name %r" % nb)
    return struct.pack(">BH", 0, len(nb)) + nb + struct.pack(">H", len(value)) + value


def encode_headers(headers):
    """Returns (count_byte_plus_headers) = Header Count followed by header bytes."""
    if len(headers) > 255:
        raise ValueError("too many headers")
    return bytes([len(headers)]) + b"".join(encode_header(n, v) for n, v in headers)


def decode_headers(buf, off, count):
    """Decode `count` headers from buf at off.
    Returns (list of (hid, name, value_bytes), new_offset). name is None for unknown IDs.
    Raises Malformed on overrun."""
    out = []
    for _ in range(count):
        if off + 1 > len(buf):
            raise Malformed("header ID past end")
        hid = buf[off]
        off += 1
        name = None
        if hid == 0:
            if off + 2 > len(buf):
                raise Malformed("name length past end")
            (nlen,) = struct.unpack_from(">H", buf, off)
            off += 2
            if not 1 <= nlen <= 255:
                raise Malformed("custom name length %d" % nlen)
            if off + nlen > len(buf):
                raise Malformed("name past end")
            nb = buf[off:off + nlen]
            off += nlen
            name = nb.decode("latin-1")
        else:
            name = HEADER_TABLE.get(hid)  # None: unknown ID, ignored by caller
        if off + 2 > len(buf):
            raise Malformed("value length past end")
        (vlen,) = struct.unpack_from(">H", buf, off)
        off += 2
        if off + vlen > len(buf):
            raise Malformed("value past end")
        out.append((hid, name, bytes(buf[off:off + vlen])))
        off += vlen
    return out, off


# ------------------------------------------------------------ message codecs
def encode_request(path, headers=(), method=METHOD_GET):
    pb = path.encode("utf-8") if isinstance(path, str) else bytes(path)
    if not 1 <= len(pb) <= MAX_PATH:
        raise ValueError("path length %d" % len(pb))
    payload = struct.pack(">BH", method, len(pb)) + pb + encode_headers(headers)
    if len(payload) > MAX_PAYLOAD:
        raise ValueError("REQUEST does not fit one frame")
    return payload


def decode_response(payload):
    """-> (status, headers[(hid,name,value)]). Raises Malformed."""
    if len(payload) < 3:
        raise Malformed("RESPONSE shorter than 3 bytes")
    status, count = struct.unpack_from(">HB", payload, 0)
    headers, end = decode_headers(payload, 3, count)
    if end != len(payload):
        raise Malformed("%d trailing bytes in RESPONSE" % (len(payload) - end))
    return status, headers


def encode_response(status, headers=()):
    """Only for building test doubles."""
    return struct.pack(">H", status) + encode_headers(headers)


def decode_error(payload):
    """-> (code, msg_bytes) ; (None, b'') if malformed (still an ERROR)."""
    if len(payload) < 4:
        return None, b""
    code, mlen = struct.unpack_from(">HH", payload, 0)
    if 4 + mlen != len(payload):
        return None, payload[4:]
    return code, payload[4:4 + mlen]


def encode_error(code, msg=b""):
    if isinstance(msg, str):
        msg = msg.encode("utf-8")
    return struct.pack(">HH", code, len(msg)) + msg


def hexdump(data, base=0):
    lines = []
    for i in range(0, len(data), 16):
        ch = data[i:i + 16]
        lines.append("%08x  %-47s  |%s|" % (
            base + i, " ".join("%02x" % b for b in ch),
            "".join(chr(b) if 32 <= b < 127 else "." for b in ch)))
    return "\n".join(lines)


# ------------------------------------------------------------------- client
class Response:
    def __init__(self):
        self.stream_id = None
        self.status = None
        self.headers = []      # (hid, name, value)
        self.body = b""
        self.frames = []       # every known frame received for this exchange (Frame objects)

    def header_ids(self):
        return [h[0] for h in self.headers]

    def get_all(self, name):
        return [v for (_, n, v) in self.headers if n == name]


class Client:
    """One persistent TCP connection; stream IDs 1,2,3,..."""

    def __init__(self, host, port, timeout=10.0, sock=None):
        if sock is None:
            sock = socket.create_connection((host, port), timeout=timeout)
        self.sock = sock
        self.next_stream = 1
        self.broken = False
        self.sent = []   # raw bytes of each send
        self.trace_rx = None

    # -- low level
    def send_raw(self, data):
        self.sent.append(bytes(data))
        write_all(self.sock, data)

    def send_frame(self, type_, flags, stream_id, payload=b"", length=None, reserved=0):
        self.send_raw(pack_frame(type_, flags, stream_id, payload, length, reserved))

    def read_frame(self, skip_unknown=True):
        return read_frame(self.sock, skip_unknown)

    def read_until_close(self, max_frames=16, skip_unknown=False):
        """Return list of frames until EOF/reset (best effort, for crafted tests).
        Last element may be an exception instance describing how reading ended."""
        out = []
        for _ in range(max_frames):
            try:
                f = read_frame(self.sock, skip_unknown)
            except (BhttpError, OSError) as e:
                out.append(e)
                return out
            if f is None:
                return out
            out.append(f)
        return out

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass

    def fail(self, exc, send_error=None):
        """A failure makes the connection unusable (SPEC 12.8: no reuse, no silent reconnect).
        If send_error is set: ERROR, FIN, drain input up to ~1 s (SPEC 9.3), then close."""
        self.broken = True
        if send_error is not None:
            try:
                self.send_raw(pack_frame(T_ERROR, 0, 0, encode_error(send_error, b"protocol error")))
                self.sock.shutdown(socket.SHUT_WR)
                self._drain(1.0)
            except (OSError, AttributeError):
                pass
        self.close()
        raise exc

    def _drain(self, seconds):
        import time
        end = time.monotonic() + seconds
        try:
            while time.monotonic() < end:
                if hasattr(self.sock, "settimeout"):
                    self.sock.settimeout(max(0.01, end - time.monotonic()))
                if not self.sock.recv(4096):
                    return
        except (OSError, AttributeError):
            return

    # -- request/response
    def alloc_stream(self):
        sid = self.next_stream
        if sid > 0xFFFFFFFF:      # SPEC 5.2: no wraparound; report failure and close
            self.fail(BhttpError("stream IDs exhausted"))
        self.next_stream += 1
        return sid

    def send_request(self, path, headers=(), method=METHOD_GET, flags=END_STREAM, stream_id=None):
        if self.broken:
            raise BhttpError("connection is broken; will not reconnect silently")
        sid = self.alloc_stream() if stream_id is None else stream_id
        self.send_frame(T_REQUEST, flags, sid, encode_request(path, headers, method))
        return sid

    def get(self, path, headers=()):
        sid = self.send_request(path, headers)
        return self.read_response(sid)

    def read_response(self, stream_id):
        """SPEC section 12 client rules."""
        resp = Response()
        resp.stream_id = stream_id
        got_response = False
        done = False
        chunks = []
        while not done:
            try:
                f = read_frame(self.sock)      # skips unknown types
            except FrameFault as e:
                self.fail(e, send_error=e.code if e.reply else None)
            except Truncated as e:
                self.fail(e)
            except OSError as e:          # reset/timeout: failure (SPEC 9.3, 12.8, 14)
                self.fail(IncompleteResponse("socket error: %s" % e))
            if f is None:
                self.fail(IncompleteResponse("EOF before END_STREAM"))
            if f.type == T_ERROR:
                code, msg = decode_error(f.payload)
                self.fail(PeerError(code, msg))         # MUST NOT reply
            if f.type == T_REQUEST:
                self.fail(ProtocolFailure("REQUEST received by client"), send_error=E_PROTOCOL_ERROR)
            if f.stream_id != stream_id:
                self.fail(ProtocolFailure("frame for stream %d, expected %d" % (f.stream_id, stream_id)))
            # SPEC 3: undefined flag bits on RESPONSE/DATA at a client are ignored.
            resp.frames.append(f)
            if f.type == T_RESPONSE:
                if got_response:
                    self.fail(ProtocolFailure("second RESPONSE on stream"))
                try:
                    resp.status, resp.headers = decode_response(f.payload)
                except Malformed as e:
                    self.fail(ProtocolFailure("bad RESPONSE: %s" % e))
                if not 100 <= resp.status <= 599:
                    self.fail(ProtocolFailure("invalid status %d" % resp.status))
                got_response = True
            else:  # DATA
                if not got_response:
                    self.fail(ProtocolFailure("DATA before RESPONSE"))
                chunks.append(f.payload)
            done = f.end_stream
        resp.body = b"".join(chunks)
        for v in resp.get_all("content-length"):
            if not v or not all(48 <= b <= 57 for b in v) or int(v) != len(resp.body):
                # SPEC 12.8: invalid response -> exchange failed, connection closed. No ERROR sent.
                self.fail(ProtocolFailure("content-length %r != DATA total %d" % (v, len(resp.body))))
        return resp
