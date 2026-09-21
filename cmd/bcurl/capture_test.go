package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"
)

// recorder sits between bcurl and the server and copies one connection's
// bytes in each direction without looking at them.
type recorder struct {
	addr string
	sent bytes.Buffer // client to server
	recv bytes.Buffer // server to client
	done chan struct{}
}

func startRecorder(t *testing.T, target string) *recorder {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{addr: l.Addr().String(), done: make(chan struct{})}
	t.Cleanup(func() { l.Close() })

	go func() {
		defer close(r.done)
		client, err := l.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		server, err := net.Dial("tcp", target)
		if err != nil {
			return
		}
		defer server.Close()

		finished := make(chan struct{}, 2)
		go func() {
			io.Copy(server, io.TeeReader(client, &r.sent))
			server.(*net.TCPConn).CloseWrite()
			finished <- struct{}{}
		}()
		go func() {
			io.Copy(client, io.TeeReader(server, &r.recv))
			client.(*net.TCPConn).CloseWrite()
			finished <- struct{}{}
		}()
		<-finished
		<-finished
	}()
	return r
}

var dumpHexRow = regexp.MustCompile(`^\s+[0-9A-F]{8}  ([0-9A-F ]+?)\s+\|`)

// dumpedBytes rebuilds, from the verbose text alone, the bytes shown for one
// direction: every frame's header line and every payload row.
func dumpedBytes(dump, arrow string) []byte {
	var out []byte
	inDirection := false
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "--> frame"), strings.HasPrefix(line, "<-- frame"):
			inDirection = strings.HasPrefix(line, arrow)
		case !inDirection:
		case strings.HasPrefix(trimmed, "header "):
			out = append(out, parseHex(strings.TrimPrefix(trimmed, "header"))...)
		default:
			if m := dumpHexRow.FindStringSubmatch(line); m != nil {
				out = append(out, parseHex(m[1])...)
			}
		}
	}
	return out
}

func parseHex(s string) []byte {
	var b []byte
	for _, f := range strings.Fields(s) {
		var v byte
		fmt.Sscanf(f, "%02X", &v)
		b = append(b, v)
	}
	return b
}

// The verbose output must be a complete picture of the connection: put a
// recorder on the wire and check that the dump, read back, is exactly what it
// recorded, for a body that spans many frames.
func TestVerboseDumpIsExactlyTheRecordedBytes(t *testing.T) {
	addr, binary := startServer(t)
	rec := startRecorder(t, addr)

	got := runBcurl(t, "-v", "-H", "x-check: full", rec.addr+"/binary.bin")
	if got.code != 0 || !bytes.Equal(got.stdout, binary) {
		t.Fatalf("exit %d, stdout %d bytes (want %d)", got.code, len(got.stdout), len(binary))
	}
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the recorder never finished")
	}

	if strings.Contains(got.stderr, "not shown") {
		t.Error("the verbose output leaves bytes out")
	}
	if n := rec.recv.Len(); n < len(binary) {
		t.Fatalf("recorder saw only %d response bytes for a %d byte body", n, len(binary))
	}
	if shown := dumpedBytes(got.stderr, "-->"); !bytes.Equal(shown, rec.sent.Bytes()) {
		t.Errorf("request direction: dump holds %d bytes, recorder saw %d", len(shown), rec.sent.Len())
	}
	if shown := dumpedBytes(got.stderr, "<--"); !bytes.Equal(shown, rec.recv.Bytes()) {
		t.Errorf("response direction: dump holds %d bytes, recorder saw %d", len(shown), rec.recv.Len())
	}
}
