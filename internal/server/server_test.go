package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
)

const testTimeout = 5 * time.Second

// countingListener lets tests prove how many TCP connections a scenario used.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

type harness struct {
	srv  *Server
	lis  *countingListener
	root string
}

func (h *harness) addr() string { return h.lis.Addr().String() }

func binaryBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// allByteValues covers every byte value, including the ones text handling
// tends to mangle, repeated so it spans more than one frame.
func allByteValues() []byte {
	var b []byte
	for i := 0; i < 100; i++ {
		for v := 0; v < 256; v++ {
			b = append(b, byte(v))
		}
	}
	return append(b, '\r', '\n', 0, '\r', '\n')
}

func startServer(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "www")
	writeFile(t, filepath.Join(root, "index.html"), []byte("<h1>home</h1>"))
	writeFile(t, filepath.Join(root, "css", "style.css"), []byte("body{color:red}"))
	writeFile(t, filepath.Join(root, "docs", "index.html"), []byte("docs page"))
	writeFile(t, filepath.Join(root, "empty.txt"), nil)
	writeFile(t, filepath.Join(root, "binary.bin"), allByteValues())
	writeFile(t, filepath.Join(root, "pic.png"), []byte("\x89PNG\r\n\x1a\n"))
	writeFile(t, filepath.Join(root, "notes.weird"), []byte("x"))
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "secret.txt"), []byte("top secret"))

	cfg := Config{Root: root}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl := &countingListener{Listener: l}
	go srv.Serve(cl)
	t.Cleanup(func() { srv.Close() })
	return &harness{srv: srv, lis: cl, root: root}
}

type testClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	next uint32
}

func dial(t *testing.T, h *harness) *testClient {
	t.Helper()
	conn, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testClient{t: t, conn: conn, br: bufio.NewReader(conn), next: 1}
}

func (c *testClient) send(f frame.Frame) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(testTimeout))
	if err := frame.Write(c.conn, f); err != nil {
		c.t.Fatalf("sending %+v: %v", f.Type, err)
	}
}

func (c *testClient) sendRaw(b []byte) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("raw write: %v", err)
	}
}

func requestFrame(t testing.TB, stream uint32, path string) frame.Frame {
	t.Helper()
	payload, err := protocol.Request{Method: protocol.MethodGet, Path: path}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: stream, Payload: payload}
}

// sendGet writes a request without waiting for the answer.
func (c *testClient) sendGet(path string) uint32 {
	c.t.Helper()
	id := c.next
	c.next++
	c.send(requestFrame(c.t, id, path))
	return id
}

type reply struct {
	status  int
	headers []protocol.Header
	body    []byte
	frames  []frame.Frame // RESPONSE first, then every DATA frame
}

func (r reply) header(name string) string {
	v, _ := protocol.Get(r.headers, name)
	return string(v)
}

func (c *testClient) readFrame() frame.Frame {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	f, err := frame.Read(c.br)
	if err != nil {
		c.t.Fatalf("reading a frame: %v", err)
	}
	return f
}

func (c *testClient) readReply(stream uint32) reply {
	c.t.Helper()
	f := c.readFrame()
	if f.Type != frame.TypeResponse || f.StreamID != stream {
		c.t.Fatalf("wanted a response on stream %d, got type %d stream %d", stream, f.Type, f.StreamID)
	}
	resp, err := protocol.DecodeResponse(f.Payload)
	if err != nil {
		c.t.Fatalf("decoding response: %v", err)
	}
	r := reply{status: resp.Status, headers: resp.Headers, frames: []frame.Frame{f}}
	for done := f.Flags.Has(frame.FlagEndStream); !done; {
		d := c.readFrame()
		if d.Type != frame.TypeData || d.StreamID != stream {
			c.t.Fatalf("wanted data on stream %d, got type %d stream %d", stream, d.Type, d.StreamID)
		}
		r.frames = append(r.frames, d)
		r.body = append(r.body, d.Payload...)
		done = d.Flags.Has(frame.FlagEndStream)
	}
	return r
}

func (c *testClient) get(path string) reply {
	c.t.Helper()
	return c.readReply(c.sendGet(path))
}

// expectClosed waits for the server to end the connection, tolerating a reset
// because closing with unread data can legitimately produce one.
func (c *testClient) expectClosed() {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	var b [1]byte
	n, err := c.br.Read(b[:])
	if n > 0 {
		c.t.Fatalf("server sent an unexpected byte %#x instead of closing", b[0])
	}
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		c.t.Fatalf("connection is still open: %v", err)
	}
}

// expectFault reads the connection-level error the server should send, then
// finishes our side quickly so the server's grace period ends early.
func (c *testClient) expectFault(code protocol.ErrorCode) {
	c.t.Helper()
	f := c.readFrame()
	if f.Type != frame.TypeError || f.StreamID != 0 {
		c.t.Fatalf("wanted an error frame on stream 0, got type %d stream %d", f.Type, f.StreamID)
	}
	ce, err := protocol.DecodeConnError(f.Payload)
	if err != nil || ce.Code != code {
		c.t.Fatalf("error frame = %+v, %v; want code %d", ce, err, code)
	}
	if tc, ok := c.conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	c.expectClosed()
}

func rawHeader(length uint32, typ, flags byte, reserved uint16, stream uint32) []byte {
	b := make([]byte, frame.HeaderSize)
	binary.BigEndian.PutUint32(b[0:4], length)
	b[4] = typ
	b[5] = flags
	binary.BigEndian.PutUint16(b[6:8], reserved)
	binary.BigEndian.PutUint32(b[8:12], stream)
	return b
}

func waitForNoConnections(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if h.srv.current.Load() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server still holds %d connections", h.srv.current.Load())
}

func TestServesFilesOnOneConnection(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)

	tests := []struct {
		path   string
		status int
		body   []byte
	}{
		{"/", 200, []byte("<h1>home</h1>")},
		{"/index.html", 200, []byte("<h1>home</h1>")},
		{"/css/style.css", 200, []byte("body{color:red}")},
		{"/docs/", 200, []byte("docs page")},
		{"/docs", 200, []byte("docs page")},
		{"/binary.bin", 200, allByteValues()},
		{"/empty.txt", 200, nil},
		{"/missing.html", 404, nil},
		{"/emptydir/", 404, nil},
		{"/emptydir", 404, nil},
		{"/index.html/", 404, nil},
		{"/css/missing.css", 404, nil},
		{"/index.html", 200, []byte("<h1>home</h1>")},
	}
	for _, tc := range tests {
		r := c.get(tc.path)
		if r.status != tc.status || !bytes.Equal(r.body, tc.body) {
			t.Errorf("%s: status %d body %q; want status %d body %q", tc.path, r.status, trim(r.body), tc.status, trim(tc.body))
		}
	}
	if n := h.lis.accepted.Load(); n != 1 {
		t.Errorf("used %d TCP connections, want 1", n)
	}
}

func trim(b []byte) []byte {
	if len(b) > 40 {
		return b[:40]
	}
	return b
}

func TestOutsideFileIsNeverServed(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	for _, p := range []string{"/../secret.txt", "/a/../../secret.txt", "/%2e%2e/secret.txt", "/..%2fsecret.txt"} {
		r := c.get(p)
		if r.status == 200 || bytes.Contains(r.body, []byte("secret")) {
			t.Errorf("%s leaked the outside file: status %d", p, r.status)
		}
	}
}

func TestUnsafePathsGet400AndTheConnectionSurvives(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	bad := []string{
		"/../secret.txt", "/a/../b", "/./index.html", `/a\b`, `/..\secret.txt`, "/a:b",
		"/C:/Windows/win.ini", "/index.html:stream", "/a\x00b", "/a\x01b", "//index.html", "/a//b",
		"/con", "/CON.txt", "/nul", "/aux.x", "/com1", "/lpt9",
		"/index.html.", "/index.html ", "/a?b", "/a*b", "index.html", "no-leading-slash",
	}
	for _, p := range bad {
		r := c.get(p)
		if r.status != 400 || len(r.body) != 0 {
			t.Errorf("%q: status %d with %d body bytes, want an empty 400", p, r.status, len(r.body))
		}
	}
	if r := c.get("/"); r.status != 200 {
		t.Errorf("connection unusable after bad paths: status %d", r.status)
	}
}

func TestFileSizesAreFramedCorrectly(t *testing.T) {
	h := startServer(t, nil)
	sizes := []int{0, 1, 16383, 16384, 16385, 32768, 1 << 20}
	for _, n := range sizes {
		writeFile(t, filepath.Join(h.root, fmt.Sprintf("size-%d.bin", n)), binaryBytes(n, int64(n)))
	}
	c := dial(t, h)
	for _, n := range sizes {
		want := binaryBytes(n, int64(n))
		r := c.get(fmt.Sprintf("/size-%d.bin", n))
		if r.status != 200 || !bytes.Equal(r.body, want) {
			t.Errorf("%d bytes: status %d, body mismatch (got %d bytes)", n, r.status, len(r.body))
			continue
		}
		if got := r.header(protocol.HeaderContentLength); got != fmt.Sprint(n) {
			t.Errorf("%d bytes: content-length %q", n, got)
		}

		resp := r.frames[0]
		data := r.frames[1:]
		if n == 0 {
			if !resp.Flags.Has(frame.FlagEndStream) || len(data) != 0 {
				t.Errorf("empty file: end-of-stream on response=%v, %d data frames", resp.Flags.Has(frame.FlagEndStream), len(data))
			}
			continue
		}
		if resp.Flags.Has(frame.FlagEndStream) {
			t.Errorf("%d bytes: response already ends the stream", n)
		}
		wantFrames := (n + frame.MaxPayload - 1) / frame.MaxPayload
		if len(data) != wantFrames {
			t.Errorf("%d bytes: %d data frames, want %d (no trailing empty frame)", n, len(data), wantFrames)
		}
		for i, d := range data {
			last := i == len(data)-1
			if len(d.Payload) > frame.MaxPayload || len(d.Payload) == 0 {
				t.Errorf("%d bytes: data frame %d carries %d bytes", n, i, len(d.Payload))
			}
			if d.Flags.Has(frame.FlagEndStream) != last {
				t.Errorf("%d bytes: end-of-stream on data frame %d is %v", n, i, d.Flags.Has(frame.FlagEndStream))
			}
		}
	}
}

func TestResponseHeaders(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)

	r := c.get("/index.html")
	names := []string{}
	for _, hd := range r.headers {
		names = append(names, hd.Name)
	}
	if strings.Join(names, ",") != "content-type,content-length,server" {
		t.Errorf("200 headers = %v", names)
	}
	if r.header("server") == "" {
		t.Error("no server header")
	}

	for _, p := range []string{"/missing", "/../x"} {
		r := c.get(p)
		names = names[:0]
		for _, hd := range r.headers {
			names = append(names, hd.Name)
		}
		if strings.Join(names, ",") != "content-length,server" || r.header("content-length") != "0" {
			t.Errorf("%s error headers = %v, content-length %q", p, names, r.header("content-length"))
		}
		if !r.frames[0].Flags.Has(frame.FlagEndStream) || len(r.frames) != 1 {
			t.Errorf("%s: an error response must be a single RESPONSE with end-of-stream", p)
		}
	}

	types := map[string]string{
		"/index.html":    "text/html",
		"/css/style.css": "text/css",
		"/pic.png":       "image/png",
		"/notes.weird":   "application/octet-stream",
		"/binary.bin":    "application/octet-stream",
		"/docs/":         "text/html",
	}
	for p, want := range types {
		if got := c.get(p).header("content-type"); got != want {
			t.Errorf("%s: content-type %q, want %q", p, got, want)
		}
	}
}

func TestMalformedRequestsGet400AndTheConnectionSurvives(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)

	goodPayload := requestFrame(t, 1, "/").Payload
	withFlags := func(flags frame.Flags) frame.Frame {
		return frame.Frame{Type: frame.TypeRequest, Flags: flags, StreamID: 0, Payload: goodPayload}
	}
	raw := func(payload []byte) frame.Frame {
		return frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, Payload: payload}
	}
	hdrClash := []byte{0x01, 0x00, 0x01, '/', 0x01, 0x00, 0x00, 0x0C}
	hdrClash = append(hdrClash, "content-type"...)
	hdrClash = append(hdrClash, 0x00, 0x01, 'v')

	tests := []struct {
		name string
		f    frame.Frame
	}{
		{"missing end of stream", withFlags(0)},
		{"undefined flag bit", withFlags(frame.FlagEndStream | 0x02)},
		{"only an undefined flag", withFlags(0x80)},
		{"unsupported method", raw([]byte{0x02, 0x00, 0x01, '/', 0x00})},
		{"method zero", raw([]byte{0x00, 0x00, 0x01, '/', 0x00})},
		{"empty payload", raw(nil)},
		{"path length zero", raw([]byte{0x01, 0x00, 0x00, 0x00})},
		{"path longer than allowed", raw(append([]byte{0x01, 0x10, 0x01}, append(bytes.Repeat([]byte{'a'}, 4097), 0x00)...))},
		{"path length past the payload", raw([]byte{0x01, 0x00, 0x20, '/', 0x00})},
		{"invalid utf-8 path", raw([]byte{0x01, 0x00, 0x02, '/', 0xFF, 0x00})},
		{"missing header count", raw([]byte{0x01, 0x00, 0x01, '/'})},
		{"header count without headers", raw([]byte{0x01, 0x00, 0x01, '/', 0x02})},
		{"trailing byte", raw([]byte{0x01, 0x00, 0x01, '/', 0x00, 0x00})},
		{"custom name clashes with table", raw(hdrClash)},
		{"a response sent to the server", frame.Frame{Type: frame.TypeResponse, Flags: frame.FlagEndStream, Payload: []byte{0, 200, 0}}},
		{"data sent to the server", frame.Frame{Type: frame.TypeData, Flags: frame.FlagEndStream, Payload: []byte("hello")}},
	}
	for _, tc := range tests {
		id := c.next
		c.next++
		tc.f.StreamID = id
		c.send(tc.f)
		r := c.readReply(id)
		if r.status != 400 || len(r.body) != 0 {
			t.Errorf("%s: status %d, want an empty 400", tc.name, r.status)
		}
	}
	if r := c.get("/index.html"); r.status != 200 || string(r.body) != "<h1>home</h1>" {
		t.Errorf("connection unusable after malformed requests: status %d", r.status)
	}
}

func TestUnknownFramesAreSkipped(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)

	for _, typ := range []byte{0x00, 0x05, 0x7F, 0xFF} {
		// Stream zero, nonsense flags and reserved bits: none of it may matter.
		junk := append(rawHeader(20, typ, 0xFF, 0xBEEF, 0), bytes.Repeat([]byte{0xAA}, 20)...)
		c.sendRaw(junk)
		if r := c.get("/index.html"); r.status != 200 {
			t.Errorf("type %#x: status %d after an unknown frame", typ, r.status)
		}
	}
	c.sendRaw(append(rawHeader(0, 0x09, 0, 0, 3), rawHeader(frame.MaxPayload, 0x0A, 0, 0, 3)...))
	c.sendRaw(bytes.Repeat([]byte{1}, frame.MaxPayload))
	if r := c.get("/"); r.status != 200 {
		t.Errorf("status %d after empty and maximum size unknown frames", r.status)
	}
}

func TestReservedBitsAreIgnored(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)

	req := requestFrame(t, 1, "/index.html")
	wire, err := frame.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	wire[6], wire[7] = 0xBE, 0xEF
	c.sendRaw(wire)
	if r := c.readReply(1); r.status != 200 {
		t.Fatalf("status %d with reserved bits set", r.status)
	}
}

func TestStreamIDsAreEchoed(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	for _, id := range []uint32{1, 7, 3, 3, 4000000000, 0xFFFFFFFF} {
		c.send(requestFrame(t, id, "/index.html"))
		if r := c.readReply(id); r.status != 200 {
			t.Errorf("stream %d: status %d", id, r.status)
		}
	}
}

func TestPipelinedRequestsAreAnsweredInOrder(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)

	var batch []byte
	paths := []string{"/index.html", "/missing", "/binary.bin", "/docs/", "/../x", "/css/style.css"}
	for i, p := range paths {
		wire, err := frame.Encode(requestFrame(t, uint32(i+1), p))
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, wire...)
	}
	c.sendRaw(batch)

	wantStatus := []int{200, 404, 200, 200, 400, 200}
	for i := range paths {
		r := c.readReply(uint32(i + 1))
		if r.status != wantStatus[i] {
			t.Errorf("%s: status %d, want %d", paths[i], r.status, wantStatus[i])
		}
	}
}

func TestRequestSplitAcrossManyWrites(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	wire, err := frame.Encode(requestFrame(t, 1, "/css/style.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range wire {
		c.sendRaw([]byte{b})
		time.Sleep(time.Millisecond)
	}
	if r := c.readReply(1); r.status != 200 || string(r.body) != "body{color:red}" {
		t.Fatalf("status %d body %q", r.status, r.body)
	}
}

func TestConnectionLevelFaults(t *testing.T) {
	h := startServer(t, nil)

	tests := []struct {
		name string
		wire []byte
		code protocol.ErrorCode
	}{
		{"request on stream zero", rawWithPayload(t, frame.TypeRequest, frame.FlagEndStream, 0, requestFrame(t, 1, "/").Payload), protocol.CodeInvalidStreamID},
		{"data on stream zero", rawWithPayload(t, frame.TypeData, 0, 0, []byte("x")), protocol.CodeInvalidStreamID},
		{"response on stream zero", rawWithPayload(t, frame.TypeResponse, 0, 0, []byte{0, 200, 0}), protocol.CodeInvalidStreamID},
		{"request one byte too large", rawHeader(frame.MaxPayload+1, byte(frame.TypeRequest), 1, 0, 1), protocol.CodeFrameTooLarge},
		{"unknown type too large", rawHeader(frame.MaxPayload+1, 0x05, 0, 0, 1), protocol.CodeFrameTooLarge},
		{"length near the maximum of a uint32", rawHeader(0xFFFFFFFF, byte(frame.TypeData), 0, 0, 1), protocol.CodeFrameTooLarge},
		{"plain text http", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), protocol.CodeFrameTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := dial(t, h)
			c.sendRaw(tc.wire)
			c.expectFault(tc.code)
		})
	}
}

func rawWithPayload(t testing.TB, typ frame.Type, flags frame.Flags, stream uint32, payload []byte) []byte {
	t.Helper()
	// Encode refuses stream zero for most types, so the header is built by hand.
	return append(rawHeader(uint32(len(payload)), byte(typ), byte(flags), 0, stream), payload...)
}

func TestFaultAfterEarlierRequestsStillReportsCleanly(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	if r := c.get("/"); r.status != 200 {
		t.Fatalf("status %d", r.status)
	}
	c.sendRaw(rawHeader(frame.MaxPayload+1, byte(frame.TypeData), 0, 0, 2))
	c.expectFault(protocol.CodeFrameTooLarge)
}

func TestErrorFramesFromClientsEndTheConnectionWithoutAReply(t *testing.T) {
	h := startServer(t, nil)

	payload, _ := protocol.ConnError{Code: protocol.CodeProtocolError, Message: "client gave up"}.Encode()
	tests := map[string][]byte{
		"well formed":           rawWithPayload(t, frame.TypeError, 0, 0, payload),
		"empty payload":         rawWithPayload(t, frame.TypeError, 0, 0, nil),
		"unknown code":          rawWithPayload(t, frame.TypeError, 0xFF, 7, []byte{0xFF, 0xFF, 0, 0}),
		"oversized error frame": rawHeader(frame.MaxPayload+1, byte(frame.TypeError), 0, 0, 0),
	}
	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := dial(t, h)
			c.sendRaw(wire)
			c.expectClosed()
		})
	}
}

func TestEndOfStreamHandling(t *testing.T) {
	t.Run("clean hang up between requests", func(t *testing.T) {
		h := startServer(t, nil)
		c := dial(t, h)
		if r := c.get("/"); r.status != 200 {
			t.Fatal("request failed")
		}
		c.conn.Close()
		waitForNoConnections(t, h)
	})

	wire, err := frame.Encode(requestFrame(t, 1, "/index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{1, 5, frame.HeaderSize - 1, frame.HeaderSize, frame.HeaderSize + 3, len(wire) - 1} {
		t.Run(fmt.Sprintf("cut after %d bytes", cut), func(t *testing.T) {
			h := startServer(t, nil)
			c := dial(t, h)
			c.sendRaw(wire[:cut])
			c.conn.Close()
			waitForNoConnections(t, h)
		})
	}

	t.Run("cut inside an unknown frame", func(t *testing.T) {
		h := startServer(t, nil)
		c := dial(t, h)
		c.sendRaw(append(rawHeader(50, 0x05, 0, 0, 1), 1, 2, 3))
		c.conn.Close()
		waitForNoConnections(t, h)
	})
}

func TestIdleConnectionsAreClosed(t *testing.T) {
	h := startServer(t, func(c *Config) { c.IdleTimeout = 200 * time.Millisecond })

	silent := dial(t, h)
	silent.expectClosed()

	busy := dial(t, h)
	for i := 0; i < 3; i++ {
		time.Sleep(120 * time.Millisecond)
		if r := busy.get("/"); r.status != 200 {
			t.Fatalf("request %d failed while active: status %d", i, r.status)
		}
	}
	busy.expectClosed()
}

func TestSlowDribbleDoesNotOutliveTheIdleLimit(t *testing.T) {
	h := startServer(t, func(c *Config) { c.IdleTimeout = 300 * time.Millisecond })
	c := dial(t, h)
	c.sendRaw([]byte{0, 0, 0}) // a frame that never finishes
	c.expectClosed()
}

func TestDrainIsBoundedInSizeAndTime(t *testing.T) {
	t.Run("endless sender", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()

		var written atomic.Int64
		go func() {
			chunk := make([]byte, 4096)
			for {
				n, err := client.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()

		start := time.Now()
		drain(server)
		if elapsed := time.Since(start); elapsed > drainTime+time.Second {
			t.Errorf("drain took %v", elapsed)
		}
		if got := written.Load(); got > drainBytes {
			t.Errorf("drain consumed %d bytes, limit is %d", got, drainBytes)
		}
	})

	t.Run("silent peer", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		start := time.Now()
		drain(server)
		elapsed := time.Since(start)
		if elapsed < drainTime/2 || elapsed > drainTime+time.Second {
			t.Errorf("drain took %v, expected about %v", elapsed, drainTime)
		}
	})
}

func TestServerStopsTalkingToAPeerThatKeepsSending(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	c.sendRaw(rawHeader(frame.MaxPayload+1, byte(frame.TypeRequest), 1, 0, 1))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := c.conn.Write(chunk); err != nil {
				return
			}
		}
	}()

	f := c.readFrame()
	if f.Type != frame.TypeError {
		t.Fatalf("got frame type %d, want an error frame", f.Type)
	}
	start := time.Now()
	c.conn.SetReadDeadline(time.Now().Add(testTimeout))
	io.Copy(io.Discard, c.br)
	if elapsed := time.Since(start); elapsed > drainTime+2*time.Second {
		t.Errorf("connection stayed open for %v against a peer that never stops", elapsed)
	}
}

func TestManyConnectionsAtOnce(t *testing.T) {
	h := startServer(t, nil)
	big := binaryBytes(100000, 99)
	writeFile(t, filepath.Join(h.root, "big.bin"), big)

	type want struct {
		path   string
		status int
		body   []byte
	}
	wants := []want{
		{"/", 200, []byte("<h1>home</h1>")},
		{"/missing", 404, nil},
		{"/big.bin", 200, big},
		{"/binary.bin", 200, allByteValues()},
		{"/../x", 400, nil},
		{"/docs/", 200, []byte("docs page")},
	}

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", h.addr())
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			defer conn.Close()
			br := bufio.NewReader(conn)
			for n := 0; n < 20; n++ {
				w := wants[(i+n)%len(wants)]
				id := uint32(n + 1)
				if err := frame.Write(conn, requestFrame(t, id, w.path)); err != nil {
					t.Errorf("client %d: %v", i, err)
					return
				}
				status, body, err := readOne(conn, br, id)
				if err != nil || status != w.status || !bytes.Equal(body, w.body) {
					t.Errorf("client %d request %d (%s): status %d, %d body bytes, err %v", i, n, w.path, status, len(body), err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// readOne is a goroutine-safe cousin of readReply, which needs *testing.T
// methods that may only run on the test's own goroutine.
func readOne(conn net.Conn, br *bufio.Reader, stream uint32) (int, []byte, error) {
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	f, err := frame.Read(br)
	if err != nil {
		return 0, nil, err
	}
	if f.Type != frame.TypeResponse || f.StreamID != stream {
		return 0, nil, fmt.Errorf("unexpected frame type %d on stream %d", f.Type, f.StreamID)
	}
	resp, err := protocol.DecodeResponse(f.Payload)
	if err != nil {
		return 0, nil, err
	}
	var body []byte
	for done := f.Flags.Has(frame.FlagEndStream); !done; {
		d, err := frame.Read(br)
		if err != nil {
			return 0, nil, err
		}
		if d.Type != frame.TypeData || d.StreamID != stream {
			return 0, nil, fmt.Errorf("unexpected data frame type %d on stream %d", d.Type, d.StreamID)
		}
		body = append(body, d.Payload...)
		done = d.Flags.Has(frame.FlagEndStream)
	}
	return resp.Status, body, nil
}

func TestCloseEndsOpenConnections(t *testing.T) {
	h := startServer(t, nil)
	c := dial(t, h)
	if r := c.get("/"); r.status != 200 {
		t.Fatal("request failed")
	}
	done := make(chan struct{})
	go func() { h.srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Close did not return")
	}
	c.expectClosed()
}

func TestNewRejectsBadRoots(t *testing.T) {
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("missing root accepted")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, file, []byte("x"))
	if _, err := New(Config{Root: file}); err == nil {
		t.Error("file accepted as root")
	}
}
