package chaos

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bhttp/internal/client"
	"bhttp/internal/frame"
	"bhttp/internal/protocol"
	"bhttp/internal/server"
)

func fixtureRoot() string { return filepath.Join("..", "..", "conformance", "www") }

func startBackend(t *testing.T) string {
	t.Helper()
	srv, err := server.New(server.Config{Root: fixtureRoot()})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func startProxy(t *testing.T, target string, o Options) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Serve(l, target, o)
	t.Cleanup(func() { l.Close() })
	return l.Addr().String()
}

func TestWriteChunkedRespectsTheLimitAndKeepsEveryByte(t *testing.T) {
	data := make([]byte, 5000)
	rand.New(rand.NewSource(1)).Read(data)

	for _, max := range []int{1, 2, 7, 100, 4999, 5000, 9000} {
		var pieces [][]byte
		w := writerFunc(func(p []byte) (int, error) {
			pieces = append(pieces, append([]byte(nil), p...))
			return len(p), nil
		})
		var writes int
		if err := writeChunked(w, data, max, 0, rand.New(rand.NewSource(int64(max))), &writes); err != nil {
			t.Fatal(err)
		}

		var joined []byte
		for _, p := range pieces {
			if len(p) == 0 || len(p) > max {
				t.Fatalf("max %d: a piece of %d bytes", max, len(p))
			}
			joined = append(joined, p...)
		}
		if !bytes.Equal(joined, data) {
			t.Fatalf("max %d: the pieces do not add up to the input", max)
		}
		if writes != len(pieces) {
			t.Errorf("max %d: counted %d writes for %d pieces", max, writes, len(pieces))
		}
		if max == 1 && len(pieces) != len(data) {
			t.Errorf("one byte at a time gave %d pieces", len(pieces))
		}
	}

	var whole int
	var calls int
	writeChunked(writerFunc(func(p []byte) (int, error) { calls++; whole = len(p); return len(p), nil }), data, 0, 0, rand.New(rand.NewSource(1)), new(int))
	if calls != 1 || whole != len(data) {
		t.Errorf("with no limit expected a single write, got %d", calls)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func fetchSums(t *testing.T, addr string, paths []string) map[string][32]byte {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(conn)
	c.Timeout = 20 * time.Second
	defer c.Close()

	out := map[string][32]byte{}
	for _, p := range paths {
		var body bytes.Buffer
		res, err := c.Get(p, &body)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if res.Status != 200 {
			t.Fatalf("%s: status %d", p, res.Status)
		}
		out[p] = sha256.Sum256(body.Bytes())
	}
	return out
}

func TestAWellBehavedClientSurvivesEveryMode(t *testing.T) {
	backend := startBackend(t)
	paths := []string{"/", "/index.html", "/all-bytes.bin", "/edge-16383.bin", "/edge-16384.bin", "/edge-16385.bin", "/edge-65536.bin", "/empty.txt", "/pixel.png", "/unicode/naïve.txt"}
	want := fetchSums(t, backend, paths)

	// Delays are left out here: on some systems even a microsecond sleep lasts a
	// millisecond, which turns a 64 KiB file sent byte by byte into minutes.
	modes := map[string]Options{
		"one byte at a time":  {Chunk: 1},
		"small random chunks": {Chunk: 9},
		"unknown frames":      {Noise: true},
		"chunks and unknown":  {Chunk: 5, Noise: true},
		"large chunks, noise": {Chunk: 4000, Noise: true},
		"pass-through":        {},
	}
	for name, o := range modes {
		t.Run(name, func(t *testing.T) {
			o.Seed = 99
			got := fetchSums(t, startProxy(t, backend, o), paths)
			for _, p := range paths {
				if got[p] != want[p] {
					t.Errorf("%s: the body changed on its way through the proxy", p)
				}
			}
		})
	}
}

func TestDelaysBetweenPiecesAreHonouredOnSmallFiles(t *testing.T) {
	backend := startBackend(t)
	small := []string{"/", "/pixel.png", "/empty.txt", "/all-bytes.bin"}
	want := fetchSums(t, backend, small)

	start := time.Now()
	got := fetchSums(t, startProxy(t, backend, Options{Chunk: 4, Delay: time.Millisecond, Noise: true, Seed: 3}), small)
	if time.Since(start) < 50*time.Millisecond {
		t.Errorf("finished in %v; the pauses between pieces were not applied", time.Since(start))
	}
	for _, p := range small {
		if got[p] != want[p] {
			t.Errorf("%s: the body changed", p)
		}
	}
}

// Reads the raw frames the proxy sends, including the unknown ones, to show
// that noise really is injected and that the real frames are untouched.
func TestNoiseAddsUnknownFramesAndLeavesRealOnesAlone(t *testing.T) {
	backend := startBackend(t)
	proxy := startProxy(t, backend, Options{Noise: true, Seed: 5})

	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload, _ := protocol.Request{Method: protocol.MethodGet, Path: "/edge-16385.bin"}.Encode()
	if err := frame.Write(conn, frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	var known, unknown int
	var lastWasEnd bool
	for {
		hdr := make([]byte, frame.HeaderSize)
		if _, err := io.ReadFull(br, hdr); err != nil {
			t.Fatalf("read: %v (after %d known and %d unknown frames)", err, known, unknown)
		}
		body := make([]byte, binary.BigEndian.Uint32(hdr[0:4]))
		if _, err := io.ReadFull(br, body); err != nil {
			t.Fatal(err)
		}
		if hdr[4] >= 1 && hdr[4] <= 4 {
			known++
			lastWasEnd = hdr[5]&byte(frame.FlagEndStream) != 0
			if lastWasEnd {
				break
			}
		} else {
			unknown++
		}
	}
	// The response is RESPONSE + two DATA frames; every one gets noise before it.
	if known != 3 || unknown < 3 {
		t.Errorf("%d known frames and %d unknown frames", known, unknown)
	}
}

func TestNoiseAfterTheLastFrameIsSkippedOnTheNextRequest(t *testing.T) {
	backend := startBackend(t)
	proxy := startProxy(t, backend, Options{Noise: true, Seed: 11})
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(conn)
	c.Timeout = 5 * time.Second
	defer c.Close()

	for i := 0; i < 5; i++ {
		var body bytes.Buffer
		res, err := c.Get("/index.html", &body)
		if err != nil || res.Status != 200 || body.Len() == 0 {
			t.Fatalf("request %d: %+v %v", i+1, res, err)
		}
	}
}

func TestServerErrorsAndClosesPassThrough(t *testing.T) {
	backend := startBackend(t)
	proxy := startProxy(t, backend, Options{Chunk: 3, Delay: 10 * time.Microsecond})

	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	f, err := frame.Read(conn)
	if err != nil || f.Type != frame.TypeError {
		t.Fatalf("expected the server's error frame through the proxy: %+v, %v", f, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	if _, err := io.ReadAll(conn); err != nil {
		t.Logf("after the error the connection ended with %v (a reset is acceptable)", err)
	}
}

func TestProxyPassesThroughStreamsItCannotFrame(t *testing.T) {
	// A backend that answers with something that is not the protocol at all.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("HTTP/1.1 200 OK\r\n\r\nhello"))
	}()

	proxy := startProxy(t, l.Addr().String(), Options{Noise: true, Chunk: 2})
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := io.ReadAll(conn)
	if !bytes.Equal(got, []byte("HTTP/1.1 200 OK\r\n\r\nhello")) {
		t.Errorf("got %q", got)
	}
}

func TestMain(m *testing.M) {
	if _, err := os.Stat(fixtureRoot()); err != nil {
		panic("conformance fixture missing: " + err.Error())
	}
	os.Exit(m.Run())
}

// A conformance tool is only useful if it catches bad clients. These two make
// the classic mistakes: assuming one Read returns a whole frame header, and
// treating a frame type they do not know as an error.

func sendIndexRequest(t *testing.T, conn net.Conn) {
	t.Helper()
	payload, _ := protocol.Request{Method: protocol.MethodGet, Path: "/index.html"}.Encode()
	if err := frame.Write(conn, frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
}

// naiveOneRead expects the first Read to deliver the whole 12-byte header.
func naiveOneRead(t *testing.T, addr string) bool {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sendIndexRequest(t, conn)

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	return err == nil && n >= frame.HeaderSize && buf[4] == byte(frame.TypeResponse)
}

// strictAboutTypes reads exactly, but fails on any type it does not know.
func strictAboutTypes(t *testing.T, addr string) bool {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sendIndexRequest(t, conn)

	br := bufio.NewReader(conn)
	for {
		hdr := make([]byte, frame.HeaderSize)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return false
		}
		if _, err := io.ReadFull(br, make([]byte, binary.BigEndian.Uint32(hdr[0:4]))); err != nil {
			return false
		}
		if hdr[4] < 1 || hdr[4] > 4 {
			return false
		}
		if hdr[5]&byte(frame.FlagEndStream) != 0 {
			return true
		}
	}
}

func TestTheProxyCatchesClientsThatBreakTheFramingRules(t *testing.T) {
	backend := startBackend(t)

	if !naiveOneRead(t, backend) {
		t.Fatal("the naive reader should work when the server is reached directly")
	}
	if naiveOneRead(t, startProxy(t, backend, Options{Chunk: 1, Delay: 2 * time.Millisecond, Seed: 1})) {
		t.Error("a client that assumes one Read is one frame header got through byte-at-a-time delivery")
	}

	if !strictAboutTypes(t, backend) {
		t.Fatal("the strict reader should work when the server is reached directly")
	}
	if strictAboutTypes(t, startProxy(t, backend, Options{Noise: true, Seed: 1})) {
		t.Error("a client that rejects unknown frame types got through the noise")
	}
}
