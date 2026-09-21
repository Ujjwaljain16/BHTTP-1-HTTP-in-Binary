package wire

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"

	"bhttp/internal/frame"
)

func encode(t *testing.T, f frame.Frame) []byte {
	t.Helper()
	b, err := frame.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDumpShowsHeaderFieldsAndExactBytes(t *testing.T) {
	wire := encode(t, frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: 1, Payload: []byte("/index.html")})

	var out bytes.Buffer
	tap := NewTap(&out)
	tap.feed(&tap.sent, wire)

	got := out.String()
	for _, want := range []string{
		"--> frame 1  REQUEST  stream=1  flags=END_STREAM  length=11",
		fmt.Sprintf("header   % X", wire[:frame.HeaderSize]),
		"/index.html",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dump is missing %q:\n%s", want, got)
		}
	}
	// The payload row must carry the real bytes, not a re-encoding.
	if !strings.Contains(got, fmt.Sprintf("% X", wire[frame.HeaderSize:frame.HeaderSize+8])) {
		t.Errorf("payload bytes not shown as sent:\n%s", got)
	}
}

func TestBoundariesDoNotChangeTheDump(t *testing.T) {
	var stream []byte
	stream = append(stream, encode(t, frame.Frame{Type: frame.TypeResponse, StreamID: 3, Payload: []byte{0, 200, 0}})...)
	stream = append(stream, encode(t, frame.Frame{Type: frame.TypeData, StreamID: 3, Payload: bytes.Repeat([]byte{0xAB}, 40)})...)
	stream = append(stream, encode(t, frame.Frame{Type: frame.TypeData, Flags: frame.FlagEndStream, StreamID: 3, Payload: []byte{1}})...)

	render := func(chunk int) string {
		var out bytes.Buffer
		tap := NewTap(&out)
		for i := 0; i < len(stream); i += chunk {
			tap.feed(&tap.recv, stream[i:min(i+chunk, len(stream))])
		}
		tap.Flush()
		return out.String()
	}
	whole := render(len(stream))
	for _, chunk := range []int{1, 3, 11, 12, 13, 50} {
		if got := render(chunk); got != whole {
			t.Errorf("chunk size %d changed the dump", chunk)
		}
	}
	if strings.Count(whole, "<-- frame") != 3 {
		t.Errorf("expected three frames:\n%s", whole)
	}
}

var hexRow = regexp.MustCompile(`^\s+[0-9A-F]{8}  ([0-9A-F ]+?)\s+\|`)

// dumped rebuilds the bytes a dump claims were seen going one way, using only
// the dump's text, so it can be compared with what really crossed the wire.
func dumped(dump, arrow string) []byte {
	var out []byte
	var inDirection bool
	for _, line := range strings.Split(dump, "\n") {
		switch {
		case strings.HasPrefix(line, "--> frame"), strings.HasPrefix(line, "<-- frame"):
			inDirection = strings.HasPrefix(line, arrow)
		case !inDirection:
		case strings.HasPrefix(strings.TrimSpace(line), "header"):
			out = append(out, unhex(strings.TrimPrefix(strings.TrimSpace(line), "header"))...)
		default:
			if m := hexRow.FindStringSubmatch(line); m != nil {
				out = append(out, unhex(m[1])...)
			}
		}
	}
	return out
}

func unhex(s string) []byte {
	var b []byte
	for _, f := range strings.Fields(s) {
		var v byte
		fmt.Sscanf(f, "%02X", &v)
		b = append(b, v)
	}
	return b
}

func TestEveryPayloadByteIsPrinted(t *testing.T) {
	for _, size := range []int{1, 255, 256, 257, 5000, frame.MaxPayload} {
		for _, typ := range []frame.Type{frame.TypeData, frame.TypeResponse, frame.TypeRequest} {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i*31 + 7)
			}
			wire := encode(t, frame.Frame{Type: typ, StreamID: 1, Payload: payload})

			var out bytes.Buffer
			tap := NewTap(&out)
			tap.feed(&tap.recv, wire)

			if strings.Contains(out.String(), "not shown") {
				t.Errorf("type %d, %d bytes: part of the frame was left out", typ, size)
			}
			if got := dumped(out.String(), "<--"); !bytes.Equal(got, wire) {
				t.Errorf("type %d, %d bytes: the dump holds %d bytes, want the %d that were received", typ, size, len(got), len(wire))
			}
		}
	}
}

func TestUnknownTypesAndFlagsAreLabelled(t *testing.T) {
	var out bytes.Buffer
	tap := NewTap(&out)
	raw := encode(t, frame.Frame{Type: 0x05, Flags: 0x81, StreamID: 9, Payload: []byte{1}})
	tap.feed(&tap.recv, raw)
	if got := out.String(); !strings.Contains(got, "unknown(0x05)") || !strings.Contains(got, "END_STREAM|0x80") {
		t.Errorf("labels missing:\n%s", got)
	}
}

func TestIncompleteFrameIsReportedOnFlush(t *testing.T) {
	var out bytes.Buffer
	tap := NewTap(&out)
	wire := encode(t, frame.Frame{Type: frame.TypeData, StreamID: 1, Payload: make([]byte, 30)})
	tap.feed(&tap.sent, wire[:20])
	if out.Len() != 0 {
		t.Fatalf("printed a partial frame early:\n%s", out.String())
	}
	tap.Flush()
	if !strings.Contains(out.String(), "incomplete frame at end of connection (20 bytes)") {
		t.Errorf("flush did not report the leftover bytes:\n%s", out.String())
	}
}

func TestGarbageDoesNotStopTheDump(t *testing.T) {
	var out bytes.Buffer
	tap := NewTap(&out)
	tap.feed(&tap.recv, []byte("HTTP/1.1 200 OK\r\n\r\nhello"))
	tap.feed(&tap.recv, []byte("more"))
	got := out.String()
	if !strings.Contains(got, "not a valid frame") || !strings.Contains(got, "bytes after an unreadable frame") {
		t.Errorf("garbage was not reported:\n%s", got)
	}
}

func TestWrappedConnectionReportsBothDirectionsInOrder(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	var out bytes.Buffer
	tap := NewTap(&out)
	conn := tap.Wrap(client)
	defer conn.Close()

	req := encode(t, frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: 1, Payload: []byte("/x")})
	resp := encode(t, frame.Frame{Type: frame.TypeResponse, Flags: frame.FlagEndStream, StreamID: 1, Payload: []byte{1, 148, 0}})

	go func() {
		buf := make([]byte, len(req))
		server.Read(buf)
		server.Write(resp)
	}()
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(resp))
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	sent := strings.Index(got, "--> frame 1  REQUEST")
	recv := strings.Index(got, "<-- frame 2  RESPONSE")
	if sent < 0 || recv < 0 || sent > recv {
		t.Errorf("expected the request then the response:\n%s", got)
	}
	if !bytes.Equal(buf, resp) {
		t.Error("the tap altered the bytes it passed through")
	}
}
