"""Checks the exact bytes a BHTTP/1 client puts on the wire.

A recording proxy sits between the client and a real server. The bytes of each
connection are then decoded here, by code that shares nothing with the client,
and compared with a request built straight from the wire format description
and, if you give one, with the request a second client sends for the same job.
The body the client wrote to stdout is compared with the DATA bytes the server
actually sent, so a client's own idea of what it received is never trusted.

    python client_wire.py --server HOST:PORT [options] -- COMMAND [ARGS...]

The server must serve the conformance files. The client is started as
COMMAND [ARGS...] HOST:PORT/path [/more ...].

options:
  --multi              the client takes several paths and fetches them in order
                       over one connection
  --no-headers         the client has no -H "name: value" option; skips the
                       header cases
  --reference CMD      a second client command, shell-quoted, whose request
                       bytes must match byte for byte for single-path cases
  --ok-code N          exit status for success (default 0)
  --http-error-code N  exit status when the server answered 4xx/5xx (default 1)
"""
import argparse
import shlex
import socket
import struct
import subprocess
import sys
import threading
import time

TABLE = {"content-type": 1, "content-length": 2, "server": 3, "date": 4,
         "connection": 5, "content-encoding": 6, "cache-control": 7, "last-modified": 8}


class Recorder:
    """Forwards every connection untouched and keeps the bytes of each direction."""

    def __init__(self, server):
        self.server = server
        self.lis = socket.socket()
        self.lis.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.lis.bind(("127.0.0.1", 0))
        self.lis.listen(32)
        self.port = self.lis.getsockname()[1]
        self.connections = []
        threading.Thread(target=self._accept, daemon=True).start()

    def _accept(self):
        while True:
            try:
                client, _ = self.lis.accept()
            except OSError:
                return
            record = {"sent": bytearray(), "received": bytearray()}
            self.connections.append(record)
            upstream = socket.create_connection(self.server)
            threading.Thread(target=self._pump, args=(client, upstream, record["sent"]), daemon=True).start()
            threading.Thread(target=self._pump, args=(upstream, client, record["received"]), daemon=True).start()

    @staticmethod
    def _pump(src, dst, sink):
        try:
            while True:
                chunk = src.recv(65536)
                if not chunk:
                    break
                sink += chunk
                dst.sendall(chunk)
        except OSError:
            pass
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def parse_frames(buf):
    frames, pos = [], 0
    while pos < len(buf):
        if pos + 12 > len(buf):
            raise AssertionError("the recording ends inside a frame header")
        length, ftype, flags, reserved, stream = struct.unpack_from(">IBBHI", buf, pos)
        end = pos + 12 + length
        if end > len(buf):
            raise AssertionError("the recording ends inside a frame payload")
        frames.append((ftype, flags, reserved, stream, bytes(buf[pos + 12:end])))
        pos = end
    return frames


def request_from_the_wire_format(stream, path, headers):
    payload = struct.pack(">BH", 1, len(path)) + path + struct.pack(">B", len(headers))
    for name, value in headers:
        if name in TABLE:
            payload += struct.pack(">BH", TABLE[name], len(value)) + value
        else:
            raw = name.encode()
            payload += struct.pack(">BH", 0, len(raw)) + raw + struct.pack(">H", len(value)) + value
    return struct.pack(">IBBHI", len(payload), 1, 1, 0, stream) + payload


def answers(received):
    """Per stream: status, body and whether END_STREAM was seen."""
    streams = {}
    for ftype, flags, _reserved, stream, payload in parse_frames(received):
        if ftype == 2:
            streams.setdefault(stream, {"status": struct.unpack_from(">H", payload, 0)[0], "body": b"", "end": False})
            streams[stream]["end"] |= bool(flags & 1)
        elif ftype == 3:
            streams[stream]["body"] += payload
            streams[stream]["end"] |= bool(flags & 1)
    return streams


class Checker:
    def __init__(self, args, command, recorder):
        self.args = args
        self.command = command
        self.recorder = recorder
        self.reference = shlex.split(args.reference) if args.reference else None
        self.results = []

    def check(self, name, ok, detail=""):
        self.results.append(ok)
        print("%s  %s" % ("PASS" if ok else "FAIL", name))
        if not ok and detail:
            print("        - " + detail)

    def run(self, command, extra, paths):
        before = len(self.recorder.connections)
        target = "127.0.0.1:%d%s" % (self.recorder.port, paths[0].decode("utf-8"))
        done = subprocess.run(command + extra + [target] + [p.decode("utf-8") for p in paths[1:]], capture_output=True, timeout=120)
        time.sleep(0.4)  # the recorder's threads need a moment to see the close
        connections = self.recorder.connections[before:]
        return done, connections

    def case(self, name, extra, paths, headers):
        done, connections = self.run(self.command, extra, paths)
        self.check("%s: one connection" % name, len(connections) == 1, "%d connections" % len(connections))
        if len(connections) != 1:
            return
        sent = bytes(connections[0]["sent"])
        wanted = b"".join(request_from_the_wire_format(i + 1, path, headers) for i, path in enumerate(paths))
        self.check("%s: request bytes match the wire format" % name, sent == wanted,
                   "sent   %s\n          wanted %s" % (sent[:70].hex(" "), wanted[:70].hex(" ")))
        frames = parse_frames(connections[0]["sent"])
        self.check("%s: only REQUEST frames, END_STREAM set, reserved 0, stream ids 1..%d" % (name, len(paths)),
                   [(f[0], f[1], f[2], f[3]) for f in frames] == [(1, 1, 0, i + 1) for i in range(len(paths))])

        streams = answers(connections[0]["received"])
        self.check("%s: every stream was answered and ended" % name,
                   len(streams) == len(paths) and all(s["end"] for s in streams.values()))
        bodies = b"".join(streams[i + 1]["body"] for i in range(len(paths)) if i + 1 in streams)
        self.check("%s: stdout is exactly the DATA bytes the server sent" % name, done.stdout == bodies,
                   "stdout is %d bytes, the server sent %d" % (len(done.stdout), len(bodies)))
        worst = max((s["status"] for s in streams.values()), default=0)
        want = self.args.http_error_code if worst >= 400 else self.args.ok_code
        self.check("%s: exit status %d for a worst status of %d" % (name, want, worst), done.returncode == want, "exit status %d" % done.returncode)

        if self.reference and len(paths) == 1:
            _, other = self.run(self.reference, extra, paths)
            same = len(other) == 1 and bytes(other[0]["sent"]) == sent
            self.check("%s: same request bytes as the reference client" % name, same)

    def refused(self, name, extra, target_path):
        done, connections = self.run(self.command, extra, [target_path.encode("utf-8")])
        # Opening the connection first and then refusing is fine; sending is not.
        sent = sum(len(c["sent"]) for c in connections)
        ok = done.returncode not in (self.args.ok_code, self.args.http_error_code) and sent == 0
        self.check("%s: refused before sending anything" % name, ok,
                   "exit status %d, %d byte(s) sent" % (done.returncode, sent))


def main():
    argv = sys.argv[1:]
    if "--" not in argv:
        print(__doc__)
        return 2
    cut = argv.index("--")
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--server", required=True)
    parser.add_argument("--multi", action="store_true")
    parser.add_argument("--no-headers", action="store_true")
    parser.add_argument("--reference")
    parser.add_argument("--ok-code", type=int, default=0)
    parser.add_argument("--http-error-code", type=int, default=1)
    args = parser.parse_args(argv[:cut])
    command = argv[cut + 1:]
    if not command:
        print(__doc__)
        return 2
    host, port = args.server.rsplit(":", 1)

    recorder = Recorder((host, int(port)))
    c = Checker(args, command, recorder)
    headers_ok = not args.no_headers

    print("== requests decoded from the raw bytes")
    c.case("GET /index.html", [], [b"/index.html"], [])
    c.case("GET /", [], [b"/"], [])
    if headers_ok:
        c.case("a custom header", ["-H", "x-demo: hello"], [b"/pixel.png"], [("x-demo", b"hello")])
        c.case("table headers by id, names lowercased, repeats kept in order",
               ["-H", "Content-Type: text/plain", "-H", "X-Trace: 7", "-H", "x-trace: 8"], [b"/"],
               [("content-type", b"text/plain"), ("x-trace", b"7"), ("x-trace", b"8")])
    c.case("a UTF-8 path, not percent-encoded", [], ["/unicode/naïve.txt".encode("utf-8")], [])
    for odd in [b"/a:b", b"//x", b"/../x", b"/a\\b", b"/con", b"/index.html.", b"/%2e%2e/x"]:
        c.case("the path %s sent exactly as typed" % odd.decode(), [], [odd], [])
    c.case("a 4096-byte path, the maximum", [], [b"/" + b"a" * 4095], [])
    if args.multi:
        c.case("five requests on one connection", [], [b"/index.html", b"/docs", b"/nope", b"/edge-16385.bin", b"/empty.txt"], [])
        c.case("ten requests on one connection", [],
               [b"/index.html", b"/docs", b"/assets/site.css", b"/binary.bin", b"/pixel.png", b"/empty.txt",
                b"/edge-16383.bin", b"/edge-16384.bin", b"/edge-16385.bin", b"/edge-65536.bin"], [])

    print("== requests the client must refuse itself")
    c.refused("a 4097-byte path", [], "/" + "a" * 4096)
    if headers_ok:
        c.refused("a header value that cannot fit in one frame", ["-H", "x-big: " + "v" * 20000], "/")
        c.refused("a header name with a space", ["-H", "bad name: v"], "/")

    failed = c.results.count(False)
    print("== %d passed, %d failed" % (len(c.results) - failed, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
