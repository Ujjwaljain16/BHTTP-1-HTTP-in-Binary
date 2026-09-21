// Package chaos is a TCP proxy that puts a server's answers through the two
// things a correct client must be immune to: TCP delivering bytes in arbitrary
// pieces, and frame types it has never heard of showing up between the frames
// it expects. Point a client at the proxy instead of the server; if it still
// works, its framing is sound.
package chaos

import (
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"bhttp/internal/frame"
)

type Options struct {
	// Chunk is the largest piece, in bytes, that is written toward the client
	// at once. Pieces are random between 1 and Chunk. Zero leaves writes alone.
	Chunk int
	// Delay is the pause after each piece written toward the client, which
	// keeps small pieces from being merged back together by the network stack.
	Delay time.Duration
	// Noise inserts frames of unknown types before every frame the server
	// sends, and after the last frame of every stream.
	Noise bool
	// Seed makes the randomness repeatable.
	Seed int64
	// Logf receives one line per connection. Nil silences it.
	Logf func(format string, args ...any)
}

// Serve accepts connections on l and relays each to target until l is closed.
func Serve(l net.Listener, target string, o Options) error {
	var n int64
	for {
		client, err := l.Accept()
		if err != nil {
			if isClosed(err) {
				return nil
			}
			return err
		}
		n++
		go relay(client, target, o, o.Seed+n)
	}
}

func isClosed(err error) bool {
	ne, ok := err.(*net.OpError)
	return ok && ne.Err != nil && ne.Err.Error() == "use of closed network connection"
}

func relay(client net.Conn, target string, o Options, seed int64) {
	defer client.Close()
	server, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		logf(o, "%v: cannot reach %s: %v", client.RemoteAddr(), target, err)
		return
	}
	defer server.Close()
	logf(o, "%v connected", client.RemoteAddr())

	var wg sync.WaitGroup
	wg.Add(2)

	// Requests go through untouched: this tool is about what the client
	// receives.
	go func() {
		defer wg.Done()
		io.Copy(server, client)
		halfClose(server)
	}()

	go func() {
		defer wg.Done()
		st := &stats{}
		p := &pump{out: client, o: o, rng: rand.New(rand.NewSource(seed)), st: st}
		p.run(server)
		halfClose(client)
		logf(o, "%v: relayed %d frames, injected %d unknown frames, %d writes", client.RemoteAddr(), st.frames, st.injected, st.writes)
	}()
	wg.Wait()
}

func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

func logf(o Options, format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

type stats struct{ frames, injected, writes int }

// pump reads frames from the server and writes them, possibly disturbed, to
// the client.
type pump struct {
	out net.Conn
	o   Options
	rng *rand.Rand
	st  *stats
}

func (p *pump) run(server io.Reader) {
	for {
		hdr := make([]byte, frame.HeaderSize)
		if _, err := io.ReadFull(server, hdr); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(hdr[0:4])
		if length > frame.MaxPayload {
			// Not something we can frame: pass everything through as it comes.
			if p.send(hdr) == nil {
				p.copyRaw(server)
			}
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(server, body); err != nil {
			p.send(append(hdr, body...))
			return
		}

		if p.o.Noise {
			if p.inject() != nil {
				return
			}
		}
		p.st.frames++
		if p.send(append(hdr, body...)) != nil {
			return
		}
		if p.o.Noise && hdr[5]&byte(frame.FlagEndStream) != 0 {
			if p.inject() != nil {
				return
			}
		}
	}
}

func (p *pump) copyRaw(server io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := server.Read(buf)
		if n > 0 && p.send(buf[:n]) != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

// unknownTypes are values no version of the protocol has assigned yet.
var unknownTypes = []byte{0x05, 0x06, 0x0A, 0x7E, 0x80, 0xFE, 0xFF}

// inject sends one frame of a type the client cannot know. Its flags, reserved
// bytes and stream ID are deliberately meaningless, because a receiver must not
// look at them.
func (p *pump) inject() error {
	payload := make([]byte, p.rng.Intn(65))
	p.rng.Read(payload)

	hdr := make([]byte, frame.HeaderSize)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	hdr[4] = unknownTypes[p.rng.Intn(len(unknownTypes))]
	hdr[5] = byte(p.rng.Intn(256))
	binary.BigEndian.PutUint16(hdr[6:8], uint16(p.rng.Intn(65536)))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(p.rng.Intn(3)))

	p.st.injected++
	return p.send(append(hdr, payload...))
}

func (p *pump) send(b []byte) error {
	return writeChunked(p.out, b, p.o.Chunk, p.o.Delay, p.rng, &p.st.writes)
}

// writeChunked writes b in pieces of random size between 1 and max, pausing
// after each. With max <= 0 it writes everything at once.
func writeChunked(w io.Writer, b []byte, max int, delay time.Duration, rng *rand.Rand, writes *int) error {
	for len(b) > 0 {
		n := len(b)
		if max > 0 {
			n = min(n, 1+rng.Intn(max))
		}
		if _, err := w.Write(b[:n]); err != nil {
			return err
		}
		*writes++
		b = b[n:]
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return nil
}
