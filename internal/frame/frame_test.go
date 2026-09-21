package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"testing"
	"testing/iotest"
)

// rawFrame builds bytes by hand so tests can produce things Encode refuses to,
// such as non-zero reserved bytes or oversized lengths.
func rawFrame(length uint32, typ, flags byte, reserved uint16, stream uint32, payload []byte) []byte {
	b := make([]byte, HeaderSize, HeaderSize+len(payload))
	binary.BigEndian.PutUint32(b[0:4], length)
	b[4] = typ
	b[5] = flags
	binary.BigEndian.PutUint16(b[6:8], reserved)
	binary.BigEndian.PutUint32(b[8:12], stream)
	return append(b, payload...)
}

func mustEncode(t testing.TB, f Frame) []byte {
	t.Helper()
	b, err := Encode(f)
	if err != nil {
		t.Fatalf("Encode(%+v): %v", f, err)
	}
	return b
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// chunkReader hands out random small slices so frame boundaries land in
// awkward places, including in the middle of the 12-byte header.
type chunkReader struct {
	r   io.Reader
	rng *rand.Rand
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	max := len(p)
	if max > 24 {
		max = 24
	}
	return c.r.Read(p[:1+c.rng.Intn(max)])
}

// The same input must decode identically however the network slices it.
var readerStyles = map[string]func(io.Reader) io.Reader{
	"whole":    func(r io.Reader) io.Reader { return r },
	"one byte": iotest.OneByteReader,
	"halves":   iotest.HalfReader,
	"random":   func(r io.Reader) io.Reader { return &chunkReader{r: r, rng: rand.New(rand.NewSource(42))} },
}

func eachReader(t *testing.T, data []byte, fn func(t *testing.T, r io.Reader)) {
	t.Helper()
	for name, wrap := range readerStyles {
		t.Run(name, func(t *testing.T) { fn(t, wrap(bytes.NewReader(data))) })
	}
}

func TestEncodeMatchesWireLayout(t *testing.T) {
	got := mustEncode(t, Frame{Type: TypeRequest, Flags: FlagEndStream, StreamID: 1, Payload: []byte("abc")})
	want := []byte{
		0x00, 0x00, 0x00, 0x03, // length
		0x01,       // type
		0x01,       // flags
		0x00, 0x00, // reserved
		0x00, 0x00, 0x00, 0x01, // stream id
		'a', 'b', 'c',
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestEncodeIsBigEndian(t *testing.T) {
	got := mustEncode(t, Frame{Type: TypeData, StreamID: 0x01020304, Payload: make([]byte, 0x0102)})
	if !bytes.Equal(got[0:4], []byte{0, 0, 0x01, 0x02}) {
		t.Errorf("length bytes = % X", got[0:4])
	}
	if !bytes.Equal(got[8:12], []byte{1, 2, 3, 4}) {
		t.Errorf("stream id bytes = % X", got[8:12])
	}
}

func TestRoundTripAcrossSizes(t *testing.T) {
	for _, size := range []int{0, 1, 11, 12, 13, 16383, MaxPayload} {
		want := Frame{Type: TypeData, Flags: FlagEndStream, StreamID: 9, Payload: patterned(size)}
		data := mustEncode(t, want)
		eachReader(t, data, func(t *testing.T, r io.Reader) {
			got, err := Read(r)
			if err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("size %d: frames differ", size)
			}
			if _, err := Read(r); err != io.EOF {
				t.Fatalf("size %d: after last frame got %v, want io.EOF", size, err)
			}
		})
	}
}

func TestEmptyStreamIsCleanEOF(t *testing.T) {
	if _, err := Read(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestSeveralFramesInOneStream(t *testing.T) {
	want := []Frame{
		{Type: TypeRequest, Flags: FlagEndStream, StreamID: 1, Payload: []byte{1, 2, 3}},
		{Type: TypeResponse, StreamID: 1, Payload: patterned(300)},
		{Type: TypeData, StreamID: 1, Payload: patterned(MaxPayload)},
		{Type: TypeData, Flags: FlagEndStream, StreamID: 1, Payload: []byte{}},
		{Type: TypeError, StreamID: 0, Payload: []byte{0, 3, 0, 0}},
	}
	var stream []byte
	for _, f := range want {
		stream = append(stream, mustEncode(t, f)...)
	}
	eachReader(t, stream, func(t *testing.T, r io.Reader) {
		for i, w := range want {
			got, err := Read(r)
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			if !reflect.DeepEqual(got, w) {
				t.Fatalf("frame %d: got %+v, want %+v", i, got, w)
			}
		}
		if _, err := Read(r); err != io.EOF {
			t.Fatalf("got %v, want io.EOF", err)
		}
	})
}

func TestMaxPayloadAcceptedAndOnePastRejected(t *testing.T) {
	if _, err := Encode(Frame{Type: TypeData, StreamID: 1, Payload: make([]byte, MaxPayload)}); err != nil {
		t.Errorf("max payload rejected: %v", err)
	}
	if _, err := Encode(Frame{Type: TypeData, StreamID: 1, Payload: make([]byte, MaxPayload+1)}); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("max+1 payload: got %v, want ErrPayloadTooLarge", err)
	}
}

// Fails the test if the reader is asked for anything beyond the header, which
// proves an oversized length is refused before any payload is awaited or allocated.
type headerOnlyReader struct {
	t    *testing.T
	data []byte
}

func (h *headerOnlyReader) Read(p []byte) (int, error) {
	if len(h.data) == 0 {
		h.t.Error("reader was asked for bytes beyond the header")
		return 0, errors.New("read past header")
	}
	n := copy(p, h.data)
	h.data = h.data[n:]
	return n, nil
}

func TestOversizeLengthRejectedBeforePayload(t *testing.T) {
	lengths := []uint32{MaxPayload + 1, 65536, 1 << 24, 0x7FFFFFFF, 0xFFFFFFFF}
	types := []byte{0x00, byte(TypeRequest), byte(TypeResponse), byte(TypeData), byte(TypeError), 0x05, 0xFF}
	for _, length := range lengths {
		for _, typ := range types {
			hdr := rawFrame(length, typ, 0, 0, 1, nil)
			_, err := Read(&headerOnlyReader{t: t, data: hdr})
			var oe *OversizeError
			if !errors.As(err, &oe) {
				t.Fatalf("length %d type %#x: got %v, want OversizeError", length, typ, err)
			}
			if oe.Type != Type(typ) || oe.Length != length {
				t.Errorf("length %d type %#x: error reports type %#x length %d", length, typ, oe.Type, oe.Length)
			}
		}
	}
}

func TestTruncatedHeader(t *testing.T) {
	full := mustEncode(t, Frame{Type: TypeRequest, StreamID: 1, Payload: []byte{1}})
	for n := 1; n < HeaderSize; n++ {
		eachReader(t, full[:n], func(t *testing.T, r io.Reader) {
			if _, err := Read(r); !errors.Is(err, ErrTruncated) {
				t.Fatalf("%d header bytes: got %v, want ErrTruncated", n, err)
			}
		})
	}
}

func TestTruncatedPayload(t *testing.T) {
	for _, have := range []int{0, 1, 50, 99} {
		data := rawFrame(100, byte(TypeData), 0, 0, 1, make([]byte, have))
		eachReader(t, data, func(t *testing.T, r io.Reader) {
			if _, err := Read(r); !errors.Is(err, ErrTruncated) {
				t.Fatalf("%d of 100 payload bytes: got %v, want ErrTruncated", have, err)
			}
		})
	}
}

func TestUnknownTypesAreSkipped(t *testing.T) {
	want := Frame{Type: TypeRequest, Flags: FlagEndStream, StreamID: 7, Payload: []byte("after")}
	valid := mustEncode(t, want)

	for _, typ := range []byte{0x00, 0x05, 0x06, 0x7F, 0xFF} {
		// Flags, reserved bytes and stream id are all nonsense on purpose:
		// nothing about an unknown frame may be interpreted.
		stream := rawFrame(8, typ, 0xFF, 0xBEEF, 0, patterned(8))
		stream = append(stream, rawFrame(0, typ, 0xFE, 0xFFFF, 0, nil)...)
		stream = append(stream, rawFrame(MaxPayload, typ, 0x00, 0, 3, patterned(MaxPayload))...)
		stream = append(stream, valid...)

		eachReader(t, stream, func(t *testing.T, r io.Reader) {
			got, err := Read(r)
			if err != nil {
				t.Fatalf("type %#x: %v", typ, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("type %#x: got %+v, want %+v", typ, got, want)
			}
		})
	}
}

func TestUnknownFrameCutShort(t *testing.T) {
	data := rawFrame(40, 0x05, 0, 0, 1, make([]byte, 10))
	if _, err := Read(bytes.NewReader(data)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v, want ErrTruncated", err)
	}
}

func TestOnlyUnknownFramesThenEOF(t *testing.T) {
	data := rawFrame(2, 0x09, 0, 0, 1, []byte{1, 2})
	if _, err := Read(bytes.NewReader(data)); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestReservedBytesAreIgnored(t *testing.T) {
	data := rawFrame(2, byte(TypeResponse), byte(FlagEndStream), 0xBEEF, 4, []byte{0, 200})
	got, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := Frame{Type: TypeResponse, Flags: FlagEndStream, StreamID: 4, Payload: []byte{0, 200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFlagsReachTheCallerUnfiltered(t *testing.T) {
	data := rawFrame(0, byte(TypeRequest), 0xF1, 0, 1, nil)
	got, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got.Flags != 0xF1 || !got.Flags.Has(FlagEndStream) {
		t.Fatalf("flags = %#x", got.Flags)
	}
}

func TestStreamZeroRules(t *testing.T) {
	tests := []struct {
		typ     Type
		stream  uint32
		wantErr error
	}{
		{TypeRequest, 0, ErrStreamZero},
		{TypeResponse, 0, ErrStreamZero},
		{TypeData, 0, ErrStreamZero},
		{TypeRequest, 1, nil},
		{TypeError, 0, nil},
		{TypeError, 5, nil},
	}
	for _, tc := range tests {
		data := rawFrame(0, byte(tc.typ), 0, 0, tc.stream, nil)
		_, err := Read(bytes.NewReader(data))
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("type %#x stream %d: got %v, want %v", tc.typ, tc.stream, err, tc.wantErr)
		}
	}
}

func TestStreamZeroFrameIsFullyConsumed(t *testing.T) {
	// The bad frame's payload is read before the error is raised, so a
	// caller that chose to keep going would still be aligned.
	bad := rawFrame(3, byte(TypeData), 0, 0, 0, []byte{1, 2, 3})
	next := mustEncode(t, Frame{Type: TypeData, StreamID: 1, Payload: []byte{9}})
	r := bytes.NewReader(append(bad, next...))
	if _, err := Read(r); !errors.Is(err, ErrStreamZero) {
		t.Fatalf("got %v, want ErrStreamZero", err)
	}
	if got, err := Read(r); err != nil || got.StreamID != 1 {
		t.Fatalf("follow-up frame: %+v, %v", got, err)
	}
}

func TestReaderPassesThroughTransportErrors(t *testing.T) {
	boom := errors.New("connection reset")
	data := mustEncode(t, Frame{Type: TypeData, StreamID: 1, Payload: patterned(50)})

	for _, cut := range []int{0, 5, HeaderSize, HeaderSize + 20} {
		r := io.MultiReader(bytes.NewReader(data[:cut]), iotest.ErrReader(boom))
		if _, err := Read(r); !errors.Is(err, boom) {
			t.Errorf("cut at %d: got %v, want the transport error", cut, err)
		}
	}
}

// shortWriter accepts at most limit bytes per call, like a congested socket.
type shortWriter struct {
	buf   bytes.Buffer
	limit int
	calls int
}

func (s *shortWriter) Write(p []byte) (int, error) {
	s.calls++
	if len(p) > s.limit {
		p = p[:s.limit]
	}
	return s.buf.Write(p)
}

func TestWriteSurvivesShortWrites(t *testing.T) {
	f := Frame{Type: TypeData, Flags: FlagEndStream, StreamID: 3, Payload: patterned(1000)}
	for _, limit := range []int{1, 5, 12, 13, 999} {
		w := &shortWriter{limit: limit}
		if err := Write(w, f); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if !bytes.Equal(w.buf.Bytes(), mustEncode(t, f)) {
			t.Fatalf("limit %d: bytes on the wire differ from the encoding", limit)
		}
	}
}

func TestWriteIsOneCallWhenWriterAllowsIt(t *testing.T) {
	w := &shortWriter{limit: 1 << 20}
	if err := Write(w, Frame{Type: TypeData, StreamID: 1, Payload: patterned(500)}); err != nil {
		t.Fatal(err)
	}
	if w.calls != 1 {
		t.Fatalf("writer called %d times, want 1", w.calls)
	}
}

type stalledWriter struct{}

func (stalledWriter) Write([]byte) (int, error) { return 0, nil }

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriteReportsStallsAndFailures(t *testing.T) {
	f := Frame{Type: TypeData, StreamID: 1, Payload: []byte{1}}
	if err := Write(stalledWriter{}, f); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("stalled writer: got %v, want io.ErrShortWrite", err)
	}
	boom := errors.New("broken pipe")
	if err := Write(failingWriter{boom}, f); !errors.Is(err, boom) {
		t.Errorf("failing writer: got %v, want the writer's error", err)
	}
}

func TestWriteRefusesInvalidFrames(t *testing.T) {
	var sink bytes.Buffer
	tests := []struct {
		name string
		f    Frame
		want error
	}{
		{"payload too large", Frame{Type: TypeData, StreamID: 1, Payload: make([]byte, MaxPayload+1)}, ErrPayloadTooLarge},
		{"request on stream zero", Frame{Type: TypeRequest, StreamID: 0}, ErrStreamZero},
		{"response on stream zero", Frame{Type: TypeResponse, StreamID: 0}, ErrStreamZero},
		{"data on stream zero", Frame{Type: TypeData, StreamID: 0}, ErrStreamZero},
	}
	for _, tc := range tests {
		if err := Write(&sink, tc.f); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if sink.Len() != 0 {
		t.Errorf("%d bytes were written for invalid frames", sink.Len())
	}
	if err := Write(&sink, Frame{Type: TypeError, StreamID: 0, Payload: []byte{0, 1, 0, 0}}); err != nil {
		t.Errorf("error frame on stream zero: %v", err)
	}
}

func TestEncodeDoesNotAliasPayload(t *testing.T) {
	payload := []byte{1, 2, 3}
	b := mustEncode(t, Frame{Type: TypeData, StreamID: 1, Payload: payload})
	payload[0] = 99
	if b[HeaderSize] != 1 {
		t.Fatal("encoded bytes changed when the caller reused its payload slice")
	}
}

type readOutcome struct {
	frames []Frame
	err    string
}

func readEverything(r io.Reader) readOutcome {
	var out readOutcome
	for {
		f, err := Read(r)
		if err != nil {
			out.err = err.Error()
			return out
		}
		out.frames = append(out.frames, f)
	}
}

func FuzzRead(f *testing.F) {
	seeds := [][]byte{
		nil,
		{0, 0, 0},
		mustEncode(f, Frame{Type: TypeRequest, Flags: FlagEndStream, StreamID: 1, Payload: []byte("hello")}),
		mustEncode(f, Frame{Type: TypeError, Payload: []byte{0, 1, 0, 0}}),
		rawFrame(4, 0x05, 0xFF, 0xFFFF, 0, []byte{1, 2, 3, 4}),
		rawFrame(0xFFFFFFFF, byte(TypeData), 0, 0, 1, nil),
		rawFrame(20, byte(TypeData), 0, 0, 1, []byte{1}),
		[]byte("GET / HTTP/1.1\r\n\r\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		direct := readEverything(bytes.NewReader(data))
		dribbled := readEverything(iotest.OneByteReader(bytes.NewReader(data)))
		if !reflect.DeepEqual(direct, dribbled) {
			t.Fatalf("read boundaries changed the result:\n whole:    %+v\n one byte: %+v", direct, dribbled)
		}

		for _, fr := range direct.frames {
			if len(fr.Payload) > MaxPayload {
				t.Fatalf("returned a %d byte payload", len(fr.Payload))
			}
			again, err := Read(bytes.NewReader(mustEncode(t, fr)))
			if err != nil || !reflect.DeepEqual(again, fr) {
				t.Fatalf("frame did not survive re-encoding: %+v -> %+v, %v", fr, again, err)
			}
		}
	})
}
