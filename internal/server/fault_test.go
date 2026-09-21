package server

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
)

func TestFileThatCannotBeOpenedGets500AndTheConnectionSurvives(t *testing.T) {
	h := startServer(t, nil)
	locked := filepath.Join(h.root, "locked.txt")
	writeFile(t, locked, []byte("cannot be read right now"))
	unlock := lockFile(t, locked)

	c := dial(t, h)
	r := c.get("/locked.txt")
	if r.status != 500 || len(r.body) != 0 {
		t.Fatalf("status %d with %d body bytes, want an empty 500", r.status, len(r.body))
	}
	if len(r.frames) != 1 || !r.frames[0].Flags.Has(frame.FlagEndStream) {
		t.Errorf("a 500 must be one RESPONSE frame carrying end-of-stream, got %d frames", len(r.frames))
	}
	if r.header(protocol.HeaderContentLength) != "0" || r.header(protocol.HeaderServer) == "" {
		t.Errorf("500 headers: content-length %q, server %q", r.header(protocol.HeaderContentLength), r.header(protocol.HeaderServer))
	}

	// The failure was about one file, not the connection.
	if r := c.get("/index.html"); r.status != 200 || string(r.body) != "<h1>home</h1>" {
		t.Errorf("next request on the same connection: status %d", r.status)
	}

	// And it is not sticky: once the file can be read again it is served.
	unlock()
	if r := c.get("/locked.txt"); r.status != 200 || string(r.body) != "cannot be read right now" {
		t.Errorf("after unlocking: status %d body %q", r.status, r.body)
	}
	if n := h.lis.accepted.Load(); n != 1 {
		t.Errorf("used %d connections, want 1", n)
	}
}

// A file that turns out shorter than promised must never look like a complete
// download. The test holds the client back so the server stalls with most of a
// large file still unread, shortens the file, then lets the client catch up.
func TestFileThatShrinksMidTransferEndsWithAnErrorNotASilentTruncation(t *testing.T) {
	h := startServer(t, nil)

	const size = 48 << 20
	path := filepath.Join(h.root, "big.bin")
	chunk := binaryBytes(1<<20, 7)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for written := 0; written < size; written += len(chunk) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	c := dial(t, h)
	c.sendGet("/big.bin")

	head := c.readFrame()
	if head.Type != frame.TypeResponse || head.Flags.Has(frame.FlagEndStream) {
		t.Fatalf("first frame: type %d flags %#x", head.Type, head.Flags)
	}
	resp, err := protocol.DecodeResponse(head.Payload)
	if err != nil || resp.Status != 200 {
		t.Fatalf("response: %+v, %v", resp, err)
	}
	if v, _ := protocol.Get(resp.Headers, protocol.HeaderContentLength); string(v) != strconv.Itoa(size) {
		t.Fatalf("announced length %q, want %d", v, size)
	}

	first := c.readFrame()
	if first.Type != frame.TypeData {
		t.Fatalf("second frame has type %d", first.Type)
	}
	received := len(first.Payload)

	// Give the server time to fill every buffer and block mid-file.
	time.Sleep(500 * time.Millisecond)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("shortening the file: %v", err)
	}

	for {
		f := c.readFrame()
		if f.Type == frame.TypeError {
			ce, err := protocol.DecodeConnError(f.Payload)
			if err != nil || ce.Code != protocol.CodeProtocolError || f.StreamID != 0 {
				t.Fatalf("error frame = %+v on stream %d, %v", ce, f.StreamID, err)
			}
			break
		}
		if f.Type != frame.TypeData || f.StreamID != 1 {
			t.Fatalf("unexpected frame: type %d stream %d", f.Type, f.StreamID)
		}
		if f.Flags.Has(frame.FlagEndStream) {
			t.Fatalf("the stream was ended cleanly after only %d of %d bytes", received+len(f.Payload), size)
		}
		received += len(f.Payload)
	}
	if received >= size {
		t.Fatalf("the whole file was already in flight (%d bytes), so nothing was truncated; make the file bigger", received)
	}

	if tc, ok := c.conn.(interface{ CloseWrite() error }); ok {
		tc.CloseWrite()
	}
	c.expectClosed()
}
