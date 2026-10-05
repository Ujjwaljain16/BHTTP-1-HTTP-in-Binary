# Interoperability checks

A protocol is only a protocol if someone who never saw the first implementation can speak it. These two programs were written that way.

| Directory | Written from | Talks to |
|---|---|---|
| `python_client` | the three files in `protocol/`, nothing else | `bserve` |
| `python_server` | the three files in `protocol/`, nothing else | `bcurl` |

Both use only the Python standard library and share no code with the Go implementation or with each other. Wherever the specification made the implementer guess, the guess was recorded and the specification was changed to remove it; the final runs are against the final text.

## Run

```
python python_client/selftest.py
python python_server/selftest.py

# Python client against bserve
bin/bserve ./www 9000
python python_client/run_interop.py 127.0.0.1 9000 --www ./www

# bcurl against the Python server
python python_server/bhttp_server.py ./www 9001 127.0.0.1
bin/bcurl 127.0.0.1:9001/index.html
BHTTP_PEER=127.0.0.1:9001 BHTTP_PEER_ROOT=./www go test -run External ./internal/client
```

`www` needs an `index.html` and a `binary.bin`. The client run checks `/`, `/index.html`, the binary file (by SHA-256), a missing file, malformed requests, an unknown frame type, a non-zero Reserved field, stream ID 0 and an oversized length, all on real connections, and reports every check.

`logs/` holds the output of the runs that back the claims in the top-level README.

## Tools for checking any client

Two scripts check a client written by anyone, given its command after `--`. They are described in section 9 and 10 of `docs/INTEROP_GUIDE.md`, and our own client runs against both in its tests.

| Script | What it does |
|---|---|
| `client_attack.py` | plays a server that misbehaves in one way at a time (wrong stream ids, lying lengths, ERROR frames, oversized frames, truncation, resets) and judges the client's exit status, its ERROR frames and whether it stays on one connection |
| `client_wire.py` | records the bytes the client sends through a proxy and compares them with a request built from the wire format, and its stdout with the DATA bytes the server sent |

```
python client_attack.py --multi --timeout-flag=-t -- python path/to/bcurl.py
python client_wire.py --server 127.0.0.1:9000 --multi -- python path/to/bcurl.py
```

`logs/classmate-client-vs-bserve.txt` and `logs/classmate-client-attacks.txt` are the runs of a classmate's client (Pratyush Mishra's `bcurl.py`) against `bserve` and through these tools, summarised in `logs/classmate-client-results.png`.
