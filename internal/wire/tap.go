// Package wire shows what actually crossed a connection. It watches the raw
// bytes going each way and prints them as hexdumps split into frames, so what
// you see is what was sent, not a re-encoding of what the program thinks it sent.
package wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"bhttp/internal/frame"
)

// dataPreview limits how much of each DATA payload is printed. Bodies can be
// megabytes; the frame header and the first bytes are what make a dump useful.
const dataPreview = 256

// Tap prints every frame seen in either direction, in the order it happened.
type Tap struct {
	mu   sync.Mutex
	out  io.Writer
	sent framer
	recv framer
	seq  int
}

func NewTap(out io.Writer) *Tap {
	t := &Tap{out: out}
	t.sent = framer{tap: t, arrow: "-->"}
	t.recv = framer{tap: t, arrow: "<--"}
	return t
}

// Wrap returns a connection whose traffic is reported to the tap.
func (t *Tap) Wrap(c net.Conn) net.Conn { return &tappedConn{Conn: c, tap: t} }

// Flush reports bytes that never added up to a whole frame, which is exactly
// what you want to see when a peer hangs up mid-frame.
func (t *Tap) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent.flush()
	t.recv.flush()
}

type tappedConn struct {
	net.Conn
	tap *Tap
}

func (c *tappedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.tap.feed(&c.tap.recv, p[:n])
	}
	return n, err
}

func (c *tappedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.tap.feed(&c.tap.sent, p[:n])
	}
	return n, err
}

// CloseWrite keeps half-closing available; embedding net.Conn would hide it.
func (c *tappedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (t *Tap) feed(f *framer, b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f.feed(b)
}

// framer reassembles one direction of the byte stream into frames. It only
// reads the length field to find boundaries; it never changes the bytes.
type framer struct {
	tap    *Tap
	arrow  string
	buf    []byte
	broken bool
}

func (f *framer) feed(b []byte) {
	if f.broken {
		f.printRaw("bytes after an unreadable frame", b)
		return
	}
	f.buf = append(f.buf, b...)
	for len(f.buf) >= frame.HeaderSize {
		length := binary.BigEndian.Uint32(f.buf[0:4])
		if length > frame.MaxPayload {
			f.broken = true
			f.printRaw(fmt.Sprintf("not a valid frame (length field says %d bytes)", length), f.buf)
			f.buf = nil
			return
		}
		total := frame.HeaderSize + int(length)
		if len(f.buf) < total {
			return
		}
		f.printFrame(f.buf[:total])
		f.buf = f.buf[total:]
	}
}

func (f *framer) flush() {
	if len(f.buf) > 0 {
		f.printRaw("incomplete frame at end of connection", f.buf)
		f.buf = nil
	}
}

func (f *framer) printFrame(b []byte) {
	t := f.tap
	t.seq++
	hdr, payload := b[:frame.HeaderSize], b[frame.HeaderSize:]
	typ := frame.Type(hdr[4])

	fmt.Fprintf(t.out, "%s frame %d  %s  stream=%d  flags=%s  length=%d\n",
		f.arrow, t.seq, typeName(typ), binary.BigEndian.Uint32(hdr[8:12]), flagNames(hdr[5]), len(payload))
	fmt.Fprintf(t.out, "    header   % X\n", hdr)

	shown := payload
	if typ == frame.TypeData && len(shown) > dataPreview {
		shown = shown[:dataPreview]
	}
	if len(shown) > 0 {
		fmt.Fprintf(t.out, "    payload\n")
		writeRows(t.out, "      ", shown)
	}
	if len(shown) < len(payload) {
		fmt.Fprintf(t.out, "      ... %d more payload bytes not shown\n", len(payload)-len(shown))
	}
}

func (f *framer) printRaw(why string, b []byte) {
	t := f.tap
	fmt.Fprintf(t.out, "%s %s (%d bytes)\n", f.arrow, why, len(b))
	shown := b
	if len(shown) > dataPreview {
		shown = shown[:dataPreview]
	}
	writeRows(t.out, "      ", shown)
	if len(shown) < len(b) {
		fmt.Fprintf(t.out, "      ... %d more bytes not shown\n", len(b)-len(shown))
	}
}

func typeName(t frame.Type) string {
	switch t {
	case frame.TypeRequest:
		return "REQUEST"
	case frame.TypeResponse:
		return "RESPONSE"
	case frame.TypeData:
		return "DATA"
	case frame.TypeError:
		return "ERROR"
	}
	return fmt.Sprintf("unknown(0x%02X)", uint8(t))
}

func flagNames(flags uint8) string {
	if flags == 0 {
		return "none"
	}
	var names []string
	if flags&uint8(frame.FlagEndStream) != 0 {
		names = append(names, "END_STREAM")
	}
	if rest := flags &^ uint8(frame.FlagEndStream); rest != 0 {
		names = append(names, fmt.Sprintf("0x%02X", rest))
	}
	return strings.Join(names, "|")
}

// writeRows prints a classic hexdump: offset, sixteen bytes, printable text.
func writeRows(w io.Writer, indent string, b []byte) {
	for off := 0; off < len(b); off += 16 {
		end := min(off+16, len(b))
		row := b[off:end]

		var hex bytes.Buffer
		for i := 0; i < 16; i++ {
			if i == 8 {
				hex.WriteByte(' ')
			}
			if i < len(row) {
				fmt.Fprintf(&hex, "%02X ", row[i])
			} else {
				hex.WriteString("   ")
			}
		}

		text := make([]byte, len(row))
		for i, c := range row {
			if c >= 0x20 && c < 0x7F {
				text[i] = c
			} else {
				text[i] = '.'
			}
		}
		fmt.Fprintf(w, "%s%08X  %s |%s|\n", indent, off, hex.String(), text)
	}
}
