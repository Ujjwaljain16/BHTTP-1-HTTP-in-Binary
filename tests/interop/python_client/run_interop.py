"""Interop runner: python run_interop.py HOST PORT [--www DIR]"""
import argparse
import hashlib
import os
import socket
import struct
import sys

import bhttp_client as b

FAILS = 0


def check(name, ok, detail=""):
    global FAILS
    if not ok:
        FAILS += 1
    print("%s  %s%s" % ("PASS" if ok else "FAIL", name, ("  -- " + detail) if detail and not ok else ""))
    return ok


def sha(x):
    return hashlib.sha256(x).hexdigest()


def check_response_shape(label, sid, r, want_status):
    check(label + ": status %d" % want_status, r.status == want_status, "got %r" % r.status)
    check(label + ": stream id echoed on all frames", all(f.stream_id == sid for f in r.frames),
          str([f.stream_id for f in r.frames]))
    check(label + ": first frame is RESPONSE", r.frames and r.frames[0].type == b.T_RESPONSE)
    ends = [i for i, f in enumerate(r.frames) if f.end_stream]
    check(label + ": END_STREAM only on last frame", ends == [len(r.frames) - 1], "ends at %r" % ends)
    check(label + ": reserved flag bits clear", all(f.flags & 0xFE == 0 for f in r.frames))
    check(label + ": DATA <= 16384 each", all(f.length <= b.MAX_PAYLOAD for f in r.frames))
    data = [f for f in r.frames if f.type == b.T_DATA]
    check(label + ": no empty DATA frames", all(f.length > 0 for f in data))
    if want_status == 200:
        check(label + ": header IDs [1,2,3]", r.header_ids() == [1, 2, 3], str(r.header_ids()))
        cl = r.get_all("content-length")
        check(label + ": content-length == DATA total",
              len(cl) == 1 and cl[0].isdigit() and int(cl[0]) == len(r.body),
              "cl=%r total=%d" % (cl, len(r.body)))
        if r.body:
            check(label + ": RESPONSE has no END_STREAM, last DATA has it",
                  not r.frames[0].end_stream and data and data[-1].end_stream)
        else:
            check(label + ": empty body -> END_STREAM on RESPONSE, no DATA",
                  r.frames[0].end_stream and not data)
        ct = r.get_all("content-type")
        check(label + ": content-type present", len(ct) == 1 and len(ct[0]) > 0)
    else:
        check(label + ": header IDs [2,3]", r.header_ids() == [2, 3], str(r.header_ids()))
        check(label + ": content-length == '0'", r.get_all("content-length") == [b"0"])
        check(label + ": empty body, END_STREAM on RESPONSE",
              r.body == b"" and len(r.frames) == 1 and r.frames[0].end_stream)


def get_checked(c, path, want, www=None):
    sid = c.next_stream
    try:
        r = c.get(path)
    except b.BhttpError as e:
        check("GET %s: completes (%s)" % (path, type(e).__name__), False, str(e))
        return None
    check_response_shape("GET %s" % path, sid, r, want)
    return r


def expect_error(label, frames, code):
    fr = [f for f in frames if isinstance(f, b.Frame)]
    ok = bool(fr) and fr[0].type == b.T_ERROR
    check(label + ": ERROR frame received", ok, repr(frames))
    if ok:
        f = fr[0]
        c, _ = b.decode_error(f.payload)
        check(label + ": code %d" % code, c == code, "got %r" % c)
        check(label + ": stream 0, flags 0", f.stream_id == 0 and f.flags == 0, repr(f))
    # SPEC 9.3: sender half-closes and drains, so we expect ERROR then a clean EOF (no reset, no timeout)
    tail = frames[1:] if ok else frames
    check(label + ": ERROR then clean EOF (nothing after ERROR)", ok and tail == [], repr(tail))


def expect_close_silent(label, frames):
    check(label + ": connection closed, no reply", frames == [], repr(frames))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("host")
    ap.add_argument("port", type=int)
    ap.add_argument("--www")
    a = ap.parse_args()

    def fileb(name):
        if not a.www:
            return None
        with open(os.path.join(a.www, name), "rb") as fh:
            return fh.read()

    # ---- connection 1: many requests, one TCP connection
    c = b.Client(a.host, a.port)
    r_root = get_checked(c, "/", 200)
    r_idx = get_checked(c, "/index.html", 200)
    r_bin = get_checked(c, "/binary.bin", 200)
    r_miss = get_checked(c, "/missing", 404)
    if r_root and r_idx:
        check("/ body == /index.html body", r_root.body == r_idx.body)
    if a.www:
        for lab, r, fn in (("/", r_root, "index.html"), ("/index.html", r_idx, "index.html"),
                           ("/binary.bin", r_bin, "binary.bin")):
            if r is not None:
                exp = fileb(fn)
                check("%s sha256 matches %s" % (lab, fn), sha(r.body) == sha(exp),
                      "got %s (%d B) want %s (%d B)" % (sha(r.body)[:12], len(r.body), sha(exp)[:12], len(exp)))
    if r_bin is not None:
        n = sum(1 for f in r_bin.frames if f.type == b.T_DATA)
        print("INFO  /binary.bin: %d bytes in %d DATA frames" % (len(r_bin.body), n))
        if len(r_bin.body) > b.MAX_PAYLOAD:
            check("/binary.bin split into >1 DATA frames", n > 1)
    check("stream IDs used were 1..4", (c.next_stream == 5))

    # malformed REQUEST (method 0x02) on stream 5 then valid GET on stream 6, same connection
    try:
        sid = c.send_request("/index.html", method=0x02)
        r = c.read_response(sid)
        check_response_shape("malformed method 0x02", sid, r, 400)
    except b.BhttpError as e:
        check("malformed method: 400 and connection kept", False, str(e))
    if not c.broken:
        r = get_checked(c, "/index.html", 200)
        if r and a.www:
            check("GET after 400 still correct", sha(r.body) == sha(fileb("index.html")))
    c.close()

    # ---- extra crafted 400s on fresh connections, each followed by a valid GET
    def crafted(label, payload=None, flags=b.END_STREAM, reserved=0):
        cc = b.Client(a.host, a.port)
        try:
            pl = payload if payload is not None else b.encode_request("/index.html")
            cc.send_frame(b.T_REQUEST, flags, 1, pl, reserved=reserved)
            r = cc.read_response(1)
            check_response_shape(label, 1, r, 400)
            cc.next_stream = 2
            r2 = cc.get("/index.html")
            check(label + ": connection still usable (GET -> 200)", r2.status == 200)
        except b.BhttpError as e:
            check(label, False, "%s: %s" % (type(e).__name__, e))
        cc.close()

    crafted("request without END_STREAM", flags=0)
    crafted("request with undefined flag bit 0x80", flags=b.END_STREAM | 0x80)
    crafted("request with trailing byte", payload=b.encode_request("/index.html") + b"\x00")
    crafted("request with path traversal /../x", payload=b.encode_request("/../x"))

    # reserved field must be ignored -> 200
    cc = b.Client(a.host, a.port)
    try:
        cc.send_frame(b.T_REQUEST, b.END_STREAM, 1, b.encode_request("/index.html"), reserved=0xBEEF)
        r = cc.read_response(1)
        check("nonzero Reserved ignored (200)", r.status == 200, str(r.status))
    except b.BhttpError as e:
        check("nonzero Reserved ignored (200)", False, str(e))
    cc.close()

    # ---- unknown frame type 0x05 before a valid request
    cc = b.Client(a.host, a.port)
    try:
        # flags/reserved/stream deliberately nonsense: must not be examined
        cc.send_frame(0x05, 0xFF, 0, b"opaque-\x00\xff-payload", reserved=0x1234)
        r = cc.get("/index.html")
        check_response_shape("after unknown frame type 0x05", 1, r, 200)
    except b.BhttpError as e:
        check("unknown frame skipped, then GET works", False, "%s: %s" % (type(e).__name__, e))
    cc.close()

    # ---- REQUEST with stream ID 0 -> ERROR(2) then close
    cc = b.Client(a.host, a.port)
    cc.send_frame(b.T_REQUEST, b.END_STREAM, 0, b.encode_request("/index.html"))
    expect_error("REQUEST stream 0", cc.read_until_close(), b.E_INVALID_STREAM_ID)
    cc.close()

    # ---- Length 16385 header -> ERROR(1) then close
    cc = b.Client(a.host, a.port)
    cc.send_raw(struct.pack(">IBBHI", 16385, b.T_DATA, 0, 0, 1))
    expect_error("Length 16385", cc.read_until_close(), b.E_FRAME_TOO_LARGE)
    cc.close()

    print("\n%s (%d failed checks)" % ("FAILED" if FAILS else "ALL PASSED", FAILS))
    return 1 if FAILS else 0


if __name__ == "__main__":
    sys.exit(main())
