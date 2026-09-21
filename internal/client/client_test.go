package client

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
	"bhttp/internal/server"
)

func startRealServer(t *testing.T) (addr string, root string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", []byte("<h1>hi</h1>"))
	all := make([]byte, 0, 70000)
	for len(all) < 70000 {
		for v := 0; v < 256; v++ {
			all = append(all, byte(v))
		}
	}
	write("binary.bin", all)
	write("empty.txt", nil)

	srv, err := server.New(server.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String(), root
}

func connect(t *testing.T, addr string) *Client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := New(conn)
	c.Timeout = 3 * time.Second
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSeveralRequestsShareOneConnection(t *testing.T) {
	addr, root := startRealServer(t)
	c := connect(t, addr)

	wantBinary, err := os.ReadFile(filepath.Join(root, "binary.bin"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path   string
		status int
		body   []byte
	}{
		{"/", 200, []byte("<h1>hi</h1>")},
		{"/binary.bin", 200, wantBinary},
		{"/missing", 404, nil},
		{"/a:b", 400, nil},
		{"/empty.txt", 200, nil},
		{"/index.html", 200, []byte("<h1>hi</h1>")},
	}
	for _, tc := range tests {
		var body bytes.Buffer
		res, err := c.Get(tc.path, &body)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if res.Status != tc.status || !bytes.Equal(body.Bytes(), tc.body) || res.BodyBytes != int64(len(tc.body)) {
			t.Errorf("%s: status %d, %d body bytes; want status %d, %d bytes", tc.path, res.Status, body.Len(), tc.status, len(tc.body))
		}
	}
}

// scripted runs a fake server that handles exactly one connection.
func scripted(t *testing.T, script func(conn net.Conn, req frame.Frame)) *Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := frame.Read(conn)
		if err != nil {
			return
		}
		script(conn, req)
	}()
	return connect(t, l.Addr().String())
}

func resp(t *testing.T, stream uint32, flags frame.Flags, status int, headers ...protocol.Header) frame.Frame {
	t.Helper()
	p, err := protocol.Response{Status: status, Headers: headers}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return frame.Frame{Type: frame.TypeResponse, Flags: flags, StreamID: stream, Payload: p}
}

func data(stream uint32, flags frame.Flags, b string) frame.Frame {
	return frame.Frame{Type: frame.TypeData, Flags: flags, StreamID: stream, Payload: []byte(b)}
}

func length(n string) protocol.Header { return protocol.NewHeader(protocol.HeaderContentLength, n) }

func send(t *testing.T, conn net.Conn, frames ...frame.Frame) {
	t.Helper()
	for _, f := range frames {
		if err := frame.Write(conn, f); err != nil {
			return
		}
	}
}

func TestUnexpectedServerBehaviourFailsTheExchange(t *testing.T) {
	end := frame.FlagEndStream
	tests := []struct {
		name   string
		script func(conn net.Conn, req frame.Frame)
	}{
		{"error frame from the server", func(conn net.Conn, req frame.Frame) {
			p, _ := protocol.ConnError{Code: protocol.CodeProtocolError, Message: "nope"}.Encode()
			send(t, conn, frame.Frame{Type: frame.TypeError, Payload: p})
		}},
		{"garbled error frame", func(conn net.Conn, req frame.Frame) {
			send(t, conn, frame.Frame{Type: frame.TypeError, Payload: []byte{1}})
		}},
		{"response on the wrong stream", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID+1, end, 200))
		}},
		{"data before any response", func(conn net.Conn, req frame.Frame) {
			send(t, conn, data(req.StreamID, end, "x"))
		}},
		{"a second response on the same stream", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200), resp(t, req.StreamID, end, 200))
		}},
		{"data on the wrong stream", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200), data(req.StreamID+5, end, "x"))
		}},
		{"content-length larger than the body", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200, length("10")), data(req.StreamID, end, "abc"))
		}},
		{"content-length smaller than the body", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200, length("1")), data(req.StreamID, end, "abc"))
		}},
		{"repeated content-length that disagrees", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200, length("3"), length("4")), data(req.StreamID, end, "abc"))
		}},
		{"content-length not a number", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, end, 200, length("+0")))
		}},
		{"content-length empty", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, end, 200, length("")))
		}},
		{"status out of range", func(conn net.Conn, req frame.Frame) {
			send(t, conn, frame.Frame{Type: frame.TypeResponse, Flags: end, StreamID: req.StreamID, Payload: []byte{0x02, 0x58, 0x00}})
		}},
		{"response payload with trailing bytes", func(conn net.Conn, req frame.Frame) {
			send(t, conn, frame.Frame{Type: frame.TypeResponse, Flags: end, StreamID: req.StreamID, Payload: []byte{0x00, 0xC8, 0x00, 0x00}})
		}},
		{"closes without answering", func(conn net.Conn, req frame.Frame) {}},
		{"closes halfway through the body", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200), data(req.StreamID, 0, "abc"))
		}},
		{"closes in the middle of a frame", func(conn net.Conn, req frame.Frame) {
			b, _ := frame.Encode(resp(t, req.StreamID, end, 200))
			conn.Write(b[:len(b)-1])
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := scripted(t, tc.script)
			var body bytes.Buffer
			if _, err := c.Get("/", &body); err == nil {
				t.Fatal("the exchange should have failed")
			}
			if _, err := c.Get("/", &body); !errors.Is(err, ErrBroken) {
				t.Fatalf("after a failure got %v, want ErrBroken", err)
			}
		})
	}
}

func TestRemoteErrorCarriesTheServersReason(t *testing.T) {
	c := scripted(t, func(conn net.Conn, req frame.Frame) {
		p, _ := protocol.ConnError{Code: protocol.CodeFrameTooLarge, Message: "way too big"}.Encode()
		send(t, conn, frame.Frame{Type: frame.TypeError, Payload: p})
	})
	_, err := c.Get("/", io.Discard)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != protocol.CodeFrameTooLarge || re.Message != "way too big" {
		t.Fatalf("got %v", err)
	}
}

func TestClientAnswersServerFaultsWithAnErrorFrame(t *testing.T) {
	tests := []struct {
		name string
		wire func(req frame.Frame) []byte
		want protocol.ErrorCode
	}{
		{"stream zero", func(req frame.Frame) []byte {
			return append(hdr(3, byte(frame.TypeData), 0), 'x', 'y', 'z')
		}, protocol.CodeInvalidStreamID},
		{"oversized frame", func(req frame.Frame) []byte {
			return hdr(frame.MaxPayload+1, byte(frame.TypeData), req.StreamID)
		}, protocol.CodeFrameTooLarge},
		{"a request from the server", func(req frame.Frame) []byte {
			b, _ := frame.Encode(frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: 1, Payload: []byte{1, 0, 1, '/', 0}})
			return b
		}, protocol.CodeProtocolError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan protocol.ErrorCode, 1)
			c := scripted(t, func(conn net.Conn, req frame.Frame) {
				conn.Write(tc.wire(req))
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				f, err := frame.Read(conn)
				if err != nil || f.Type != frame.TypeError {
					got <- 0
					return
				}
				ce, _ := protocol.DecodeConnError(f.Payload)
				got <- ce.Code
			})
			if _, err := c.Get("/", io.Discard); err == nil {
				t.Fatal("the exchange should have failed")
			}
			if code := <-got; code != tc.want {
				t.Errorf("server received error code %d, want %d", code, tc.want)
			}
		})
	}
}

func hdr(length uint32, typ byte, stream uint32) []byte {
	return []byte{
		byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length),
		typ, 0, 0, 0,
		byte(stream >> 24), byte(stream >> 16), byte(stream >> 8), byte(stream),
	}
}

func TestOversizedErrorFrameIsNotAnswered(t *testing.T) {
	answered := make(chan bool, 1)
	c := scripted(t, func(conn net.Conn, req frame.Frame) {
		conn.Write(hdr(frame.MaxPayload+1, byte(frame.TypeError), 0))
		conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		_, err := frame.Read(conn)
		answered <- err == nil
	})
	if _, err := c.Get("/", io.Discard); err == nil {
		t.Fatal("the exchange should have failed")
	}
	if <-answered {
		t.Error("an error frame must never be answered")
	}
}

func TestToleratedServerQuirks(t *testing.T) {
	end := frame.FlagEndStream
	tests := []struct {
		name   string
		script func(conn net.Conn, req frame.Frame)
		status int
		body   string
	}{
		{"unknown frames anywhere", func(conn net.Conn, req frame.Frame) {
			send(t, conn,
				frame.Frame{Type: 0x05, StreamID: 0, Flags: 0xFF, Payload: []byte("noise")},
				resp(t, req.StreamID, 0, 200),
				frame.Frame{Type: 0x77, StreamID: 99, Payload: nil},
				data(req.StreamID, end, "ok"))
		}, 200, "ok"},
		{"empty data frames", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200), data(req.StreamID, 0, ""), data(req.StreamID, 0, "a"), data(req.StreamID, end, ""))
		}, 200, "a"},
		{"undefined flag bits on response and data", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0x80, 200), data(req.StreamID, end|0x40, "z"))
		}, 200, "z"},
		{"non-zero reserved bits", func(conn net.Conn, req frame.Frame) {
			b, _ := frame.Encode(resp(t, req.StreamID, end, 200))
			b[6], b[7] = 0xBE, 0xEF
			conn.Write(b)
		}, 200, ""},
		{"leading zeros in content-length", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200, length("0003")), data(req.StreamID, end, "abc"))
		}, 200, "abc"},
		{"matching repeated content-length", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, 0, 200, length("3"), length("03")), data(req.StreamID, end, "abc"))
		}, 200, "abc"},
		{"unknown header ids in the response", func(conn net.Conn, req frame.Frame) {
			p := []byte{0x00, 0xC8, 0x02, 200, 0x00, 0x01, 'x', 0x02, 0x00, 0x01, '3'}
			send(t, conn, frame.Frame{Type: frame.TypeResponse, StreamID: req.StreamID, Payload: p}, data(req.StreamID, end, "abc"))
		}, 200, "abc"},
		{"1xx and 3xx are not treated as failures", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, end, 302))
		}, 302, ""},
		{"5xx is a normal result", func(conn net.Conn, req frame.Frame) {
			send(t, conn, resp(t, req.StreamID, end, 503))
		}, 503, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := scripted(t, tc.script)
			var body bytes.Buffer
			res, err := c.Get("/", &body)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.status || body.String() != tc.body {
				t.Errorf("status %d body %q; want %d %q", res.Status, body.String(), tc.status, tc.body)
			}
		})
	}
}

func TestStreamIDsCountUpFromOne(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ids := make(chan uint32, 8)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			req, err := frame.Read(conn)
			if err != nil {
				return
			}
			ids <- req.StreamID
			if !req.Flags.Has(frame.FlagEndStream) {
				return
			}
			p, _ := protocol.Response{Status: 200}.Encode()
			frame.Write(conn, frame.Frame{Type: frame.TypeResponse, Flags: frame.FlagEndStream, StreamID: req.StreamID, Payload: p})
		}
	}()
	c := connect(t, l.Addr().String())
	for i := 0; i < 3; i++ {
		if _, err := c.Get("/", io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for want := uint32(1); want <= 3; want++ {
		if got := <-ids; got != want {
			t.Errorf("request used stream %d, want %d", got, want)
		}
	}
}

func TestStreamIDsAreNeverReused(t *testing.T) {
	c := scripted(t, func(conn net.Conn, req frame.Frame) {
		send(t, conn, resp(t, req.StreamID, frame.FlagEndStream, 200))
	})
	c.next = 0xFFFFFFFF
	if _, err := c.Get("/", io.Discard); err != nil {
		t.Fatalf("last available id should work: %v", err)
	}
	if _, err := c.Get("/", io.Discard); err == nil {
		t.Fatal("a request after the last id must be refused")
	}
	if _, err := c.Get("/", io.Discard); !errors.Is(err, ErrBroken) {
		t.Fatalf("got %v, want ErrBroken", err)
	}
}

func TestUnbuildableRequestDoesNotBreakTheConnection(t *testing.T) {
	addr, _ := startRealServer(t)
	c := connect(t, addr)
	if _, err := c.Get("", io.Discard); err == nil {
		t.Fatal("empty path accepted")
	}
	if res, err := c.Get("/", io.Discard); err != nil || res.Status != 200 {
		t.Fatalf("connection should still work: %+v, %v", res, err)
	}
}

func TestSlowServerTimesOut(t *testing.T) {
	c := scripted(t, func(conn net.Conn, req frame.Frame) { time.Sleep(2 * time.Second) })
	c.Timeout = 150 * time.Millisecond
	start := time.Now()
	if _, err := c.Get("/", io.Discard); err == nil {
		t.Fatal("should have timed out")
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v to give up", time.Since(start))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestBodyWriteFailureEndsTheExchange(t *testing.T) {
	addr, _ := startRealServer(t)
	c := connect(t, addr)
	if _, err := c.Get("/index.html", failingWriter{}); err == nil {
		t.Fatal("write error was swallowed")
	}
	if _, err := c.Get("/", io.Discard); !errors.Is(err, ErrBroken) {
		t.Fatalf("got %v, want ErrBroken", err)
	}
}
