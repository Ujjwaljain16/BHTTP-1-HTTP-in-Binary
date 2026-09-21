package protocol

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustRequest(t testing.TB, r Request) []byte {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatalf("encode %+v: %v", r, err)
	}
	return b
}

// hdr assembles one header by hand: id, then either a custom name or nothing,
// then the value. It lets tests build layouts the encoder would never emit.
func hdr(id byte, name, value string) []byte {
	b := []byte{id}
	if id == 0 {
		b = appendU16(b, uint16(len(name)))
		b = append(b, name...)
	}
	b = appendU16(b, uint16(len(value)))
	return append(b, value...)
}

// reqBytes lays out a request payload from parts with no validation at all.
func reqBytes(method byte, pathLen uint16, path string, count byte, headers ...[]byte) []byte {
	b := []byte{method}
	b = appendU16(b, pathLen)
	b = append(b, path...)
	b = append(b, count)
	for _, h := range headers {
		b = append(b, h...)
	}
	return b
}

func TestRequestEncodesToDocumentedLayout(t *testing.T) {
	got := mustRequest(t, Request{Method: MethodGet, Path: "/"})
	want := []byte{0x01, 0x00, 0x01, '/', 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}

	got = mustRequest(t, Request{
		Method:  MethodGet,
		Path:    "/a",
		Headers: []Header{NewHeader(HeaderContentType, "text/html"), NewHeader("x-trace", "7")},
	})
	want = []byte{
		0x01, 0x00, 0x02, '/', 'a',
		0x02,
		0x01, 0x00, 0x09, 't', 'e', 'x', 't', '/', 'h', 't', 'm', 'l',
		0x00, 0x00, 0x07, 'x', '-', 't', 'r', 'a', 'c', 'e', 0x00, 0x01, '7',
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X\nwant % X", got, want)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	tests := []Request{
		{Method: MethodGet, Path: "/"},
		{Method: MethodGet, Path: "/index.html"},
		{Method: MethodGet, Path: "/naïve/日本語.txt"},
		{Method: MethodGet, Path: "/" + strings.Repeat("a", MaxPathLen-1)},
		{Method: MethodGet, Path: "/x", Headers: []Header{
			NewHeader(HeaderServer, "test"),
			NewHeader("x-one", "1"),
			NewHeader("x-one", "2"),
			{Name: HeaderContentLength, Value: nil},
			{Name: "x-bin", Value: []byte{0, 255, 10, 13}},
		}},
	}
	for _, want := range tests {
		got, err := DecodeRequest(mustRequest(t, want))
		if err != nil {
			t.Fatalf("%q: %v", want.Path, err)
		}
		if got.Method != want.Method || got.Path != want.Path || len(got.Headers) != len(want.Headers) {
			t.Fatalf("%q: got %+v", want.Path, got)
		}
		for i, h := range want.Headers {
			if got.Headers[i].Name != h.Name || !bytes.Equal(got.Headers[i].Value, h.Value) {
				t.Fatalf("%q: header %d is %+v, want %+v", want.Path, i, got.Headers[i], h)
			}
		}
	}
}

func TestDecodeRequestRejectsMalformedPayloads(t *testing.T) {
	longPath := "/" + strings.Repeat("a", MaxPathLen)
	tests := []struct {
		name    string
		payload []byte
	}{
		{"empty payload", nil},
		{"only a method", []byte{0x01}},
		{"method and half a length", []byte{0x01, 0x00}},
		{"method zero", reqBytes(0x00, 1, "/", 0)},
		{"method two", reqBytes(0x02, 1, "/", 0)},
		{"method 0xFF", reqBytes(0xFF, 1, "/", 0)},
		{"path length zero", reqBytes(0x01, 0, "", 0)},
		{"path longer than the limit", reqBytes(0x01, uint16(len(longPath)), longPath, 0)},
		{"path length past the payload", reqBytes(0x01, 50, "/short", 0)},
		{"missing header count", []byte{0x01, 0x00, 0x01, '/'}},
		{"invalid utf-8 overlong", reqBytes(0x01, 3, "/\xC0\x80", 0)},
		{"invalid utf-8 lone continuation", reqBytes(0x01, 2, "/\x80", 0)},
		{"invalid utf-8 surrogate", reqBytes(0x01, 4, "/\xED\xA0\x80", 0)},
		{"header count exceeds headers present", reqBytes(0x01, 1, "/", 3, hdr(1, "", "a"), hdr(2, "", "1"))},
		{"header value runs past payload", append(reqBytes(0x01, 1, "/", 1), 0x01, 0x00, 0x10, 'a')},
		{"header cut before its length", append(reqBytes(0x01, 1, "/", 1), 0x01, 0x00)},
		{"custom name cut short", append(reqBytes(0x01, 1, "/", 1), 0x00, 0x00, 0x09, 'a')},
		{"trailing byte after headers", append(reqBytes(0x01, 1, "/", 0), 0x00)},
		{"trailing byte after last header", append(reqBytes(0x01, 1, "/", 1, hdr(1, "", "a")), 0x00)},
		{"custom name empty", reqBytes(0x01, 1, "/", 1, hdr(0, "", "v"))},
		{"custom name uppercase", reqBytes(0x01, 1, "/", 1, hdr(0, "X-Trace", "v"))},
		{"custom name with space", reqBytes(0x01, 1, "/", 1, hdr(0, "x trace", "v"))},
		{"custom name with colon", reqBytes(0x01, 1, "/", 1, hdr(0, "x:y", "v"))},
		{"custom name over 255 bytes", reqBytes(0x01, 1, "/", 1, hdr(0, strings.Repeat("a", 256), "v"))},
		{"custom name clashes with table", reqBytes(0x01, 1, "/", 1, hdr(0, "content-type", "v"))},
		{"custom name clashes with server", reqBytes(0x01, 1, "/", 1, hdr(0, "server", "v"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeRequest(tc.payload)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

func TestDecodeRequestBoundaryPathLengths(t *testing.T) {
	ok := "/" + strings.Repeat("a", MaxPathLen-1)
	if _, err := DecodeRequest(reqBytes(0x01, uint16(len(ok)), ok, 0)); err != nil {
		t.Errorf("path of exactly %d bytes rejected: %v", MaxPathLen, err)
	}
}

func TestUnknownHeaderIDsAreSkippedNotFatal(t *testing.T) {
	payload := reqBytes(0x01, 1, "/", 3,
		hdr(200, "", "mystery"),
		hdr(1, "", "text/plain"),
		hdr(9, "", ""),
	)
	got, err := DecodeRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Headers) != 1 || got.Headers[0].Name != HeaderContentType {
		t.Fatalf("headers = %+v, want only content-type", got.Headers)
	}
}

func TestEveryTableHeaderRoundTrips(t *testing.T) {
	for id := 1; id < len(tableNames); id++ {
		name := tableNames[id]
		payload := reqBytes(0x01, 1, "/", 1, hdr(byte(id), "", "v"))
		got, err := DecodeRequest(payload)
		if err != nil {
			t.Fatalf("id %d: %v", id, err)
		}
		if got.Headers[0].Name != name {
			t.Errorf("id %d decoded as %q, want %q", id, got.Headers[0].Name, name)
		}
		enc := mustRequest(t, Request{Method: MethodGet, Path: "/", Headers: []Header{NewHeader(name, "v")}})
		if !bytes.Equal(enc, payload) {
			t.Errorf("%s encoded as % X, want % X", name, enc, payload)
		}
	}
}

func TestDuplicateHeadersKeepOrder(t *testing.T) {
	payload := reqBytes(0x01, 1, "/", 3, hdr(0, "x-a", "1"), hdr(0, "x-a", "2"), hdr(0, "x-a", "3"))
	got, err := DecodeRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, h := range got.Headers {
		values = append(values, string(h.Value))
	}
	if !reflect.DeepEqual(values, []string{"1", "2", "3"}) {
		t.Fatalf("values = %v", values)
	}
	if v, _ := Get(got.Headers, "x-a"); string(v) != "1" {
		t.Fatalf("Get returned %q, want the first value", v)
	}
}

func TestHeaderCountCannotForceHugeAllocation(t *testing.T) {
	payload := reqBytes(0x01, 1, "/", 255)
	if _, err := DecodeRequest(payload); !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v, want ErrMalformed", err)
	}
}

func TestManyHeadersFillingAFrame(t *testing.T) {
	var headers []Header
	for i := 0; i < MaxHeaders; i++ {
		headers = append(headers, NewHeader("x-h", strings.Repeat("v", 50)))
	}
	b, err := Request{Method: MethodGet, Path: "/", Headers: headers}.Encode()
	if err != nil {
		t.Fatalf("255 small headers: %v", err)
	}
	got, err := DecodeRequest(b)
	if err != nil || len(got.Headers) != MaxHeaders {
		t.Fatalf("decode: %d headers, %v", len(got.Headers), err)
	}

	big := make([]Header, 10)
	for i := range big {
		big[i] = Header{Name: "x-big", Value: make([]byte, 2000)}
	}
	if _, err := (Request{Method: MethodGet, Path: "/", Headers: big}).Encode(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized request: got %v, want ErrTooLarge", err)
	}
	if _, err := (Request{Method: MethodGet, Path: "/", Headers: make([]Header, MaxHeaders+1)}).Encode(); err == nil {
		t.Fatal("256 headers accepted")
	}
}

func TestEncodeRefusesBadRequests(t *testing.T) {
	tests := map[string]Request{
		"method zero":       {Method: 0, Path: "/"},
		"empty path":        {Method: MethodGet, Path: ""},
		"path too long":     {Method: MethodGet, Path: strings.Repeat("a", MaxPathLen+1)},
		"path not utf-8":    {Method: MethodGet, Path: "/\xff"},
		"uppercase header":  {Method: MethodGet, Path: "/", Headers: []Header{NewHeader("X-Upper", "v")}},
		"empty header name": {Method: MethodGet, Path: "/", Headers: []Header{NewHeader("", "v")}},
		"value over 65535":  {Method: MethodGet, Path: "/", Headers: []Header{{Name: "x-a", Value: make([]byte, 65536)}}},
	}
	for name, r := range tests {
		if _, err := r.Encode(); err == nil {
			t.Errorf("%s: encoded without complaint", name)
		}
	}
}

func TestResponseStatusBytes(t *testing.T) {
	tests := []struct {
		status int
		want   []byte
	}{
		{200, []byte{0x00, 0xC8, 0x00}},
		{400, []byte{0x01, 0x90, 0x00}},
		{404, []byte{0x01, 0x94, 0x00}},
		{500, []byte{0x01, 0xF4, 0x00}},
	}
	for _, tc := range tests {
		got, err := Response{Status: tc.status}.Encode()
		if err != nil || !bytes.Equal(got, tc.want) {
			t.Errorf("status %d: got % X (%v), want % X", tc.status, got, err, tc.want)
		}
		back, err := DecodeResponse(got)
		if err != nil || back.Status != tc.status {
			t.Errorf("status %d: decoded %+v, %v", tc.status, back, err)
		}
	}
}

func TestResponseRoundTripWithHeaders(t *testing.T) {
	want := Response{Status: 200, Headers: []Header{
		NewHeader(HeaderContentType, "application/octet-stream"),
		NewHeader(HeaderContentLength, "1024"),
		NewHeader(HeaderServer, "bserve/1"),
		{Name: "x-raw", Value: []byte{0x00, 0xFF}},
	}}
	b, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status || len(got.Headers) != len(want.Headers) {
		t.Fatalf("got %+v", got)
	}
	for i := range want.Headers {
		if got.Headers[i].Name != want.Headers[i].Name || !bytes.Equal(got.Headers[i].Value, want.Headers[i].Value) {
			t.Errorf("header %d: got %+v, want %+v", i, got.Headers[i], want.Headers[i])
		}
	}
}

func TestDecodeResponseRejectsMalformedPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"empty", nil},
		{"one byte", []byte{0x00}},
		{"status only", []byte{0x00, 0xC8}},
		{"status 99", []byte{0x00, 0x63, 0x00}},
		{"status 600", []byte{0x02, 0x58, 0x00}},
		{"status zero", []byte{0x00, 0x00, 0x00}},
		{"header promised but absent", []byte{0x00, 0xC8, 0x01}},
		{"trailing byte", []byte{0x00, 0xC8, 0x00, 0x00}},
		{"custom name length zero", append([]byte{0x00, 0xC8, 0x01}, hdr(0, "", "v")...)},
		{"value past end", []byte{0x00, 0xC8, 0x01, 0x01, 0x00, 0x05, 'a'}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeResponse(tc.payload); !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

func TestEncodeRefusesBadStatus(t *testing.T) {
	for _, s := range []int{-1, 0, 99, 600, 70000} {
		if _, err := (Response{Status: s}).Encode(); err == nil {
			t.Errorf("status %d encoded without complaint", s)
		}
	}
}

func TestConnErrorRoundTrip(t *testing.T) {
	tests := []ConnError{
		{Code: CodeFrameTooLarge, Message: "frame too large"},
		{Code: CodeInvalidStreamID},
		{Code: CodeProtocolError, Message: "héllo"},
		{Code: 999, Message: "future code"},
	}
	for _, want := range tests {
		b, err := want.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeConnError(b)
		if err != nil || got != want {
			t.Errorf("got %+v (%v), want %+v", got, err, want)
		}
	}
	b, _ := ConnError{Code: 1, Message: "ab"}.Encode()
	if !bytes.Equal(b, []byte{0x00, 0x01, 0x00, 0x02, 'a', 'b'}) {
		t.Errorf("layout = % X", b)
	}
}

func TestDecodeConnErrorRejectsMalformed(t *testing.T) {
	tests := map[string][]byte{
		"empty":            nil,
		"code only":        {0x00, 0x01},
		"length past end":  {0x00, 0x01, 0x00, 0x05, 'a'},
		"trailing garbage": {0x00, 0x01, 0x00, 0x00, 0x00},
	}
	for name, p := range tests {
		if _, err := DecodeConnError(p); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", name, err)
		}
	}
}

func TestConnErrorKeepsInvalidUTF8Message(t *testing.T) {
	got, err := DecodeConnError([]byte{0x00, 0x03, 0x00, 0x02, 0xFF, 0xFE})
	if err != nil {
		t.Fatalf("invalid utf-8 in the message must not be fatal: %v", err)
	}
	if got.Code != CodeProtocolError || len(got.Message) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestConnErrorTooLargeMessage(t *testing.T) {
	if _, err := (ConnError{Code: 3, Message: strings.Repeat("x", maxFramePayload)}).Encode(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add(reqBytes(0x01, 1, "/", 0))
	f.Add(reqBytes(0x01, 2, "/a", 2, hdr(1, "", "x"), hdr(0, "x-a", "y")))
	f.Add(reqBytes(0x02, 1, "/", 0))
	f.Add([]byte{0x01, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := DecodeRequest(data)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		again, err := req.Encode()
		if err != nil {
			t.Fatalf("accepted request could not be re-encoded: %v", err)
		}
		back, err := DecodeRequest(again)
		if err != nil || back.Path != req.Path || len(back.Headers) != len(req.Headers) {
			t.Fatalf("re-encoded request decodes differently: %+v vs %+v (%v)", back, req, err)
		}
	})
}

func FuzzDecodeResponse(f *testing.F) {
	f.Add([]byte{0x00, 0xC8, 0x00})
	f.Add([]byte{0x01, 0x94, 0x01, 0x02, 0x00, 0x01, '0'})
	f.Add([]byte{0xFF, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		resp, err := DecodeResponse(data)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		again, err := resp.Encode()
		if err != nil {
			t.Fatalf("accepted response could not be re-encoded: %v", err)
		}
		back, err := DecodeResponse(again)
		if err != nil || back.Status != resp.Status || len(back.Headers) != len(resp.Headers) {
			t.Fatalf("re-encoded response decodes differently: %+v vs %+v (%v)", back, resp, err)
		}
	})
}

func FuzzDecodeConnError(f *testing.F) {
	f.Add([]byte{0x00, 0x01, 0x00, 0x00})
	f.Add([]byte{0x00, 0x03, 0x00, 0x02, 'h', 'i'})
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := DecodeConnError(data)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		again, err := e.Encode()
		if err != nil || !bytes.Equal(again, data) {
			t.Fatalf("re-encoding changed the bytes: % X vs % X (%v)", again, data, err)
		}
	})
}
