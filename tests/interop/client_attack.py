"""Attacks a BHTTP/1 client with a server that misbehaves in one way at a time.

A well-behaved server never shows a client how it copes with faults, so this
plays the part of a bad one: wrong stream ids, lying lengths, ERROR frames,
oversized frames, truncation, resets. Each scenario states what a correct
client does, and the client is judged on three things: its exit status, whether
it answered with an ERROR frame (and which one) or just closed, and whether it
kept to a single connection.

    python client_attack.py [options] -- COMMAND [ARGS...]

The client is started as  COMMAND [ARGS...] HOST:PORT/a [/b /c]  and must write
a response body to stdout and exit with a status that tells success, an error
status from the server (4xx/5xx) and a failed exchange apart.

options:
  --multi              the client accepts several paths and fetches them in order
                       over one connection; adds the scenarios that need that
  --timeout-flag=FLAG  how to give the client a read timeout, for example
                       --timeout-flag=-t (write it with the equals sign); it is
                       passed as "FLAG 4" and enables the silent-server test
  --ok-code N          exit status for success (default 0)
  --http-error-code N  exit status when the server answered 4xx/5xx (default 1)
  --failure-code N     exit status when the exchange itself failed (default 3)
"""
import argparse
import socket
import struct
import subprocess
import sys
import threading
import time

END = 1


def frame(ftype, flags, stream, payload=b"", reserved=0, length=None):
    n = len(payload) if length is None else length
    return struct.pack(">IBBHI", n, ftype, flags, reserved, stream) + payload


def known(hid, value):
    return struct.pack(">BH", hid, len(value)) + value


def custom(name, value):
    return struct.pack(">BH", 0, len(name)) + name + struct.pack(">H", len(value)) + value


def response(stream, status=200, headers=(), flags=0, count=None, trail=b""):
    payload = struct.pack(">HB", status, len(headers) if count is None else count) + b"".join(headers) + trail
    return frame(2, flags, stream, payload)


def data(stream, body, flags=0, **kw):
    return frame(3, flags, stream, body, **kw)


def error_frame(code=3, msg=b"nope", **kw):
    return frame(4, 0, 0, struct.pack(">HH", code, len(msg)) + msg, **kw)


def length_header(n):
    return known(2, str(n).encode())


def read_exact(sock, n):
    sock.settimeout(5)
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise EOFError("the client closed before finishing its request")
        buf += chunk
    return buf


def read_request(sock):
    length, ftype, flags, _reserved, stream = struct.unpack(">IBBHI", read_exact(sock, 12))
    payload = read_exact(sock, length)
    if ftype != 1 or not flags & END:
        raise AssertionError("the client sent something other than a REQUEST with END_STREAM")
    return stream, payload


def collect(sock, window=2.5):
    """Everything the client sends until it closes or the window ends."""
    deadline = time.monotonic() + window
    buf = b""
    while time.monotonic() < deadline:
        try:
            sock.settimeout(max(0.05, deadline - time.monotonic()))
            chunk = sock.recv(4096)
        except socket.timeout:
            break
        except OSError:
            break
        if not chunk:
            break
        buf += chunk
    return buf


def parse_frames(buf):
    out, pos = [], 0
    while pos + 12 <= len(buf):
        length, ftype, flags, _r, stream = struct.unpack_from(">IBBHI", buf, pos)
        out.append((ftype, flags, stream, buf[pos + 12:pos + 12 + length]))
        pos += 12 + length
    return out


def send(*chunks):
    return lambda sock, ctx: sock.sendall(b"".join(chunks))


def send_slowly(*chunks):
    def go(sock, ctx):
        for chunk in chunks:
            sock.sendall(chunk)
            time.sleep(0.05)
    return go


def split_writes(wire, *cuts):
    def go(sock, ctx):
        pos = 0
        for cut in cuts + (len(wire),):
            sock.sendall(wire[pos:cut])
            pos = cut
            time.sleep(0.03)
    return go


def reset_midway(sock, ctx):
    sock.sendall(response(1, 200, [length_header(100)]) + data(1, b"abc"))
    time.sleep(0.2)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
    sock.close()


def stray_frame_after_the_end(sock, ctx):
    sock.sendall(response(1, 200, [length_header(2)]) + data(1, b"ok", END) + data(1, b"stray", END))
    ctx["second"] = read_request(sock)


def answer_each(*answers):
    """Answers request after request, so a client that keeps going is observed."""
    def go(sock, ctx):
        sock.sendall(answers[0])
        for i, answer in enumerate(answers[1:], start=2):
            ctx["request %d" % i] = read_request(sock)
            sock.sendall(answer)
    return go


# Each scenario: (name, what the server does after reading the first request,
# what a correct client does). The expectation keys:
#   outcome  ok | http-error | failed
#   error    None when the client must send nothing at all after the fault,
#            or the ERROR code it must send (once, on stream 0, flags 0)
#   stdout   the exact body expected (successful scenarios only)
#   paths    several paths for one run (needs --multi) and how many requests
#            the server must see
SCENARIOS = []
MULTI_ONLY = set()


def scenario(name, script, **expect):
    SCENARIOS.append((name, script, expect))


def multi(name, script, **expect):
    scenario(name, script, **expect)
    MULTI_ONLY.add(name)


# Odd but legal answers: all of these must succeed.
scenario("plain 200 with a body", send(response(1, 200, [length_header(5)]), data(1, b"hello", END)), outcome="ok", error=None, stdout=b"hello")
scenario("an empty body ends on the RESPONSE frame", send(response(1, 200, [length_header(0)], END)), outcome="ok", error=None, stdout=b"")
scenario("empty DATA frames are tolerated", send(response(1, 200, [length_header(2)]), data(1, b""), data(1, b"a"), data(1, b""), data(1, b"b", END)), outcome="ok", error=None, stdout=b"ab")
scenario("an empty DATA frame carrying END_STREAM after the body", send(response(1, 200, [length_header(3)]), data(1, b"abc"), data(1, b"", END)), outcome="ok", error=None, stdout=b"abc")
scenario("undefined flag bits and non-zero reserved bytes are ignored on RESPONSE and DATA",
         send(response(1, 200, [length_header(3)], flags=0xF0), frame(3, END | 0xFE, 1, b"xyz", reserved=0xBEEF)), outcome="ok", error=None, stdout=b"xyz")
scenario("unknown frame types are skipped wherever they appear, whatever their flags, stream and reserved bytes",
         send(frame(0x05, 0xFF, 0, b"noise", reserved=0xFFFF), response(1, 200, [length_header(2)]), frame(0x7E, 0x81, 9, b""),
              data(1, b"ok", END), frame(0xFF, 0, 0, b"after")), outcome="ok", error=None, stdout=b"ok")
scenario("unknown header ids 9 to 255 in a response are ignored",
         send(response(1, 200, [known(200, b"x"), length_header(2), known(9, b"")]), data(1, b"ok", END)), outcome="ok", error=None, stdout=b"ok")
scenario("a custom response header with an upper-case name is accepted",
         send(response(1, 200, [custom(b"X-Upper", b"v"), length_header(2)]), data(1, b"ok", END)), outcome="ok", error=None, stdout=b"ok")
scenario("a custom response header named like a table header is accepted and ignored",
         send(response(1, 200, [custom(b"content-type", b"v"), length_header(2)]), data(1, b"ok", END)), outcome="ok", error=None, stdout=b"ok")
scenario("a custom header named content-length is not the real content-length",
         send(response(1, 200, [custom(b"content-length", b"abc"), length_header(2)]), data(1, b"ok", END)), outcome="ok", error=None, stdout=b"ok")
scenario("content-length with leading zeros equals the body numerically",
         send(response(1, 200, [known(2, b"0003")]), data(1, b"abc", END)), outcome="ok", error=None, stdout=b"abc")
scenario("repeated content-length headers that agree", send(response(1, 200, [length_header(3), known(2, b"03")]), data(1, b"abc", END)), outcome="ok", error=None, stdout=b"abc")
scenario("a 3xx answer is final and is not followed", send(response(1, 302, [length_header(0)], END)), outcome="ok", error=None, stdout=b"")
scenario("a 1xx answer is final", send(response(1, 100, [length_header(0)], END)), outcome="ok", error=None, stdout=b"")
scenario("a 503 is a normal result, reported as an error status", send(response(1, 503, [length_header(0)], END)), outcome="http-error", error=None, stdout=b"")
scenario("body bytes arriving one at a time", send_slowly(response(1, 200, [length_header(4)]), data(1, b"a"), data(1, b"b"), data(1, b"cd", END)), outcome="ok", error=None, stdout=b"abcd")
scenario("a frame arriving split across several writes",
         split_writes(response(1, 200, [length_header(0)], END), 5, 14), outcome="ok", error=None, stdout=b"")
scenario("a maximum-size DATA frame", send(response(1, 200, [length_header(16384)]), data(1, b"z" * 16384, END)), outcome="ok", error=None, stdout=b"z" * 16384)

# Faults that make the byte stream untrustworthy: answer with ERROR, then close.
scenario("a DATA frame over the size limit gets ERROR(1)", send(response(1, 200), frame(3, 0, 1, b"", length=16385)), outcome="failed", error=1)
scenario("an unknown-type frame over the size limit gets ERROR(1)", send(frame(0x05, 0, 0, b"", length=16385)), outcome="failed", error=1)
scenario("a length of 0xFFFFFFFF gets ERROR(1)", send(frame(3, 0, 1, b"", length=0xFFFFFFFF)), outcome="failed", error=1)
scenario("text HTTP instead of frames gets ERROR(1)", send(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi"), outcome="failed", error=1)
scenario("a RESPONSE on stream 0 gets ERROR(2)", send(response(0, 200, [length_header(0)], END)), outcome="failed", error=2)
scenario("a DATA frame on stream 0 gets ERROR(2)", send(response(1, 200), data(0, b"x", END)), outcome="failed", error=2)
scenario("a REQUEST sent to the client gets ERROR(3)", send(frame(1, END, 1, b"\x01\x00\x01/\x00")), outcome="failed", error=3)

# An ERROR from the server is never answered, however odd it looks.
scenario("an ERROR from the server: close, no reply", send(error_frame(3)), outcome="failed", error=None)
scenario("an ERROR with an unknown code: close, no reply", send(error_frame(999, b"future")), outcome="failed", error=None)
scenario("an ERROR with an empty payload: close, no reply", send(frame(4, 0, 0, b"")), outcome="failed", error=None)
scenario("an ERROR on a non-zero stream with flags set: close, no reply", send(frame(4, 0xFF, 7, struct.pack(">HH", 1, 0), reserved=1)), outcome="failed", error=None)
scenario("an ERROR frame over the size limit is not answered either", send(frame(4, 0, 0, b"", length=16385)), outcome="failed", error=None)
scenario("an ERROR after the response and part of the body", send(response(1, 200, [length_header(10)]), data(1, b"abc"), error_frame(3)), outcome="failed", error=None)

# A bad response: the client just closes, and the exchange failed.
scenario("a RESPONSE on the wrong stream", send(response(2, 200, [length_header(0)], END)), outcome="failed", error=None)
scenario("DATA before any RESPONSE", send(data(1, b"x", END)), outcome="failed", error=None)
scenario("a second RESPONSE on the same stream", send(response(1, 200), response(1, 200, [], END)), outcome="failed", error=None)
scenario("DATA on another stream", send(response(1, 200), data(3, b"x", END)), outcome="failed", error=None)
scenario("a content-length larger than the body", send(response(1, 200, [length_header(10)]), data(1, b"abc", END)), outcome="failed", error=None)
scenario("a content-length smaller than the body", send(response(1, 200, [length_header(1)]), data(1, b"abc", END)), outcome="failed", error=None)
scenario("repeated content-length headers that disagree", send(response(1, 200, [length_header(3), length_header(4)]), data(1, b"abc", END)), outcome="failed", error=None)
scenario("a content-length that is not digits", send(response(1, 200, [known(2, b"+0")], END)), outcome="failed", error=None)
scenario("an empty content-length", send(response(1, 200, [known(2, b"")], END)), outcome="failed", error=None)
scenario("a 20-digit content-length", send(response(1, 200, [known(2, b"0" * 20)], END)), outcome="failed", error=None)
scenario("status 99", send(frame(2, END, 1, struct.pack(">HB", 99, 0))), outcome="failed", error=None)
scenario("status 600", send(frame(2, END, 1, struct.pack(">HB", 600, 0))), outcome="failed", error=None)
scenario("status 0", send(frame(2, END, 1, struct.pack(">HB", 0, 0))), outcome="failed", error=None)
scenario("a byte left over after the last header of a RESPONSE", send(response(1, 200, [length_header(0)], END, trail=b"\x00")), outcome="failed", error=None)
scenario("a header count that promises more headers than exist", send(response(1, 200, [length_header(0)], END, count=3)), outcome="failed", error=None)
scenario("a header that runs past the end of the payload", send(frame(2, END, 1, struct.pack(">HB", 200, 1) + b"\x01\x00\x09ab")), outcome="failed", error=None)
scenario("a custom header with a name length of 0", send(frame(2, END, 1, struct.pack(">HB", 200, 1) + struct.pack(">BHH", 0, 0, 0))), outcome="failed", error=None)
scenario("a custom header with a name length of 256", send(response(1, 200, [custom(b"a" * 256, b"v")], END)), outcome="failed", error=None)
scenario("a RESPONSE payload shorter than three bytes", send(frame(2, END, 1, b"\x00\xc8")), outcome="failed", error=None)

# The connection ending early.
scenario("the connection closes before any response", lambda s, c: None, outcome="failed", error=None)
scenario("the connection closes inside a frame header", lambda s, c: s.sendall(response(1, 200)[:7]), outcome="failed", error=None)
scenario("the connection closes inside a frame payload", lambda s, c: s.sendall(frame(2, 0, 1, b"", length=50)[:12] + b"x" * 10), outcome="failed", error=None)
scenario("the connection closes inside an unknown frame being skipped", lambda s, c: s.sendall(frame(0x05, 0, 0, b"", length=40)[:12] + b"x" * 5), outcome="failed", error=None)
scenario("the connection closes after the RESPONSE, before END_STREAM", send(response(1, 200, [length_header(5)])), outcome="failed", error=None)
scenario("the connection closes in the middle of the body", send(response(1, 200, [length_header(10)]), data(1, b"abc")), outcome="failed", error=None)
scenario("the connection is reset in the middle of the body", reset_midway, outcome="failed", error=None)

# Several requests on one connection.
multi("after a failure no further request is sent and nothing reconnects",
      send(response(2, 200, [length_header(0)], END)), outcome="failed", error=None, paths=["/a", "/b", "/c"], requests=1)
multi("after a 404 the later requests are still sent on the same connection",
      answer_each(response(1, 404, [length_header(0)], END), response(2, 200, [length_header(1)]) + data(2, b"z", END), response(3, 200, [length_header(0)], END)),
      outcome="http-error", error=None, paths=["/a", "/b", "/c"], requests=3, stdout=b"z")
multi("a stray frame after END_STREAM is caught when the next response is read",
      stray_frame_after_the_end, outcome="failed", error=None, paths=["/a", "/b"], requests=2, stdout=b"ok")


class Judge:
    def __init__(self, args, command):
        self.args = args
        self.command = command
        self.codes = {"ok": args.ok_code, "http-error": args.http_error_code, "failed": args.failure_code}
        self.lis = socket.socket()
        self.lis.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.lis.bind(("127.0.0.1", 0))
        self.lis.listen(8)
        self.port = self.lis.getsockname()[1]
        self.results = []

    def client_command(self, paths, read_timeout):
        cmd = list(self.command)
        if self.args.timeout_flag:
            cmd += [self.args.timeout_flag, str(read_timeout)]
        cmd.append("127.0.0.1:%d%s" % (self.port, paths[0]))
        return cmd + paths[1:]

    def run(self, name, script, expect):
        paths = expect.get("paths", ["/a"])
        ctx, view = {}, {}

        def serve():
            self.lis.settimeout(10)
            try:
                conn, _ = self.lis.accept()
            except OSError:
                return
            try:
                view["first"] = read_request(conn)
                script(conn, ctx)
                view["from_client"] = collect(conn)
            except Exception as exc:  # a scenario that cannot run is a bug here, so say so
                view["broken"] = repr(exc)
            finally:
                try:
                    conn.close()
                except OSError:
                    pass

        thread = threading.Thread(target=serve)
        thread.start()
        started = time.monotonic()
        try:
            done = subprocess.run(self.client_command(paths, 4), capture_output=True, timeout=60)
        except subprocess.TimeoutExpired:
            self.report(name, ["the client did not finish within 60 seconds"])
            thread.join(15)
            return
        took = time.monotonic() - started
        thread.join(15)

        self.lis.settimeout(0.3)
        try:
            extra, _ = self.lis.accept()
            extra.close()
            reconnected = True
        except OSError:
            reconnected = False

        problems = []
        if "broken" in view:
            problems.append("test server: " + view["broken"])
        want_code = self.codes[expect["outcome"]]
        if done.returncode != want_code:
            problems.append("exit status %d, want %d (%s)" % (done.returncode, want_code, done.stderr.decode(errors="replace").strip()[-140:]))
        if "stdout" in expect and done.stdout != expect["stdout"]:
            problems.append("body written was %r, want %r" % (done.stdout[:40], expect["stdout"][:40]))
        sent = view.get("from_client", b"")
        if expect["error"] is None:
            if sent:
                problems.append("the client sent %d unexpected bytes after the fault: %r" % (len(sent), sent[:30]))
        else:
            frames = parse_frames(sent)
            errors = [f for f in frames if f[0] == 4]
            code_ok = len(errors) == 1 and len(errors[0][3]) >= 2 and struct.unpack(">H", errors[0][3][:2])[0] == expect["error"]
            if not code_ok or errors[0][2] != 0 or errors[0][1] != 0 or len(frames) != 1:
                problems.append("wanted exactly one ERROR(%d) on stream 0 with flags 0 and nothing else, got %r" % (expect["error"], frames))
        if reconnected:
            problems.append("the client opened a second connection")
        if "requests" in expect:
            seen = 1 + sum(1 for key in ctx if key.startswith("request") or key == "second")
            if seen != expect["requests"]:
                problems.append("the server saw %d request(s), want %d" % (seen, expect["requests"]))
        if took > 15:
            problems.append("took %.1f seconds" % took)
        self.report(name, problems)

    def report(self, name, problems):
        self.results.append(not problems)
        print("%s  %s" % ("PASS" if not problems else "FAIL", name))
        for problem in problems:
            print("        - " + problem)

    def silent_server(self):
        def serve():
            self.lis.settimeout(10)
            try:
                conn, _ = self.lis.accept()
                read_request(conn)
                time.sleep(4)
                conn.close()
            except (OSError, EOFError):
                pass

        thread = threading.Thread(target=serve)
        thread.start()
        started = time.monotonic()
        done = subprocess.run(self.client_command(["/a"], 1), capture_output=True, timeout=30)
        took = time.monotonic() - started
        thread.join(15)
        ok = done.returncode == self.args.failure_code and took < 3.5
        self.results.append(ok)
        print("%s  a server that never answers: the client gives up by itself (exit %d after %.1fs)" % ("PASS" if ok else "FAIL", done.returncode, took))


def main():
    argv = sys.argv[1:]
    if "--" not in argv:
        print(__doc__)
        return 2
    cut = argv.index("--")
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--multi", action="store_true")
    parser.add_argument("--timeout-flag")
    parser.add_argument("--ok-code", type=int, default=0)
    parser.add_argument("--http-error-code", type=int, default=1)
    parser.add_argument("--failure-code", type=int, default=3)
    args = parser.parse_args(argv[:cut])
    command = argv[cut + 1:]
    if not command:
        print(__doc__)
        return 2

    judge = Judge(args, command)
    chosen = [s for s in SCENARIOS if args.multi or s[0] not in MULTI_ONLY]
    print("== %d scenarios against: %s" % (len(chosen), " ".join(command)))
    for name, script, expect in chosen:
        judge.run(name, script, expect)
    if args.timeout_flag:
        judge.silent_server()
    failed = judge.results.count(False)
    print("== %d passed, %d failed" % (len(judge.results) - failed, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
