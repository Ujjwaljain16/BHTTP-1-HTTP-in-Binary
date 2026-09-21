"""Records the raw bytes of one TCP connection between a client and a server.

    python capture_proxy.py LISTEN_PORT TARGET_HOST TARGET_PORT OUT_PREFIX

Point the client at LISTEN_PORT instead of the server. When the client
disconnects, everything it sent is in OUT_PREFIX.request.bin and everything
the server sent back is in OUT_PREFIX.response.bin. The proxy never looks at
or changes the bytes; it only copies them, which makes it an independent
witness for what the client's own verbose output claims.
"""
import socket
import sys
import threading


def pump(src, dst, sink, done):
    try:
        while True:
            chunk = src.recv(65536)
            if not chunk:
                break
            sink.append(chunk)
            dst.sendall(chunk)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass
        done.set()


def main():
    listen_port, host, port, prefix = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), sys.argv[4]

    lis = socket.socket()
    lis.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    lis.bind(("127.0.0.1", listen_port))
    lis.listen(1)
    print("recording on 127.0.0.1:%d" % lis.getsockname()[1], flush=True)

    client, _ = lis.accept()
    server = socket.create_connection((host, port))

    sent, received = [], []
    client_done, server_done = threading.Event(), threading.Event()
    threading.Thread(target=pump, args=(client, server, sent, client_done), daemon=True).start()
    threading.Thread(target=pump, args=(server, client, received, server_done), daemon=True).start()
    client_done.wait()
    server_done.wait(timeout=5)

    with open(prefix + ".request.bin", "wb") as f:
        f.write(b"".join(sent))
    with open(prefix + ".response.bin", "wb") as f:
        f.write(b"".join(received))
    print("request: %d bytes, response: %d bytes" % (sum(map(len, sent)), sum(map(len, received))), flush=True)


if __name__ == "__main__":
    main()
