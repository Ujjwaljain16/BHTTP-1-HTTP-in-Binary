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
