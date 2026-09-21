package protocol

import "fmt"

type Request struct {
	Method  Method
	Path    string
	Headers []Header
}

// Encode builds the request payload. The path is only checked for size and
// encoding here; deciding whether a server will accept it is the server's job.
func (r Request) Encode() ([]byte, error) {
	if r.Method != MethodGet {
		return nil, fmt.Errorf("unsupported method %#x", uint8(r.Method))
	}
	if len(r.Path) == 0 || len(r.Path) > MaxPathLen {
		return nil, fmt.Errorf("path length %d is outside 1..%d", len(r.Path), MaxPathLen)
	}
	if !validUTF8([]byte(r.Path)) {
		return nil, fmt.Errorf("path is not valid UTF-8")
	}

	b := make([]byte, 0, 4+len(r.Path))
	b = append(b, byte(r.Method))
	b = appendU16(b, uint16(len(r.Path)))
	b = append(b, r.Path...)
	b, err := appendHeaders(b, r.Headers)
	if err != nil {
		return nil, err
	}
	if len(b) > maxFramePayload {
		return nil, ErrTooLarge
	}
	return b, nil
}

func DecodeRequest(payload []byte) (Request, error) {
	c := &cursor{b: payload}

	method, err := c.u8()
	if err != nil {
		return Request{}, err
	}
	if Method(method) != MethodGet {
		return Request{}, malformed("method %#x is not supported", method)
	}

	pathLen, err := c.u16()
	if err != nil {
		return Request{}, err
	}
	if pathLen == 0 || pathLen > MaxPathLen {
		return Request{}, malformed("path length %d is outside 1..%d", pathLen, MaxPathLen)
	}
	path, err := c.take(int(pathLen))
	if err != nil {
		return Request{}, err
	}
	if !validUTF8(path) {
		return Request{}, malformed("path is not valid UTF-8")
	}

	headers, err := readHeaders(c)
	if err != nil {
		return Request{}, err
	}
	if err := c.finish(); err != nil {
		return Request{}, err
	}
	return Request{Method: MethodGet, Path: string(path), Headers: headers}, nil
}

type Response struct {
	Status  int
	Headers []Header
}

func validStatus(s int) bool { return s >= 100 && s <= 599 }

func (r Response) Encode() ([]byte, error) {
	if !validStatus(r.Status) {
		return nil, fmt.Errorf("status %d is outside 100..599", r.Status)
	}
	b := appendU16(make([]byte, 0, 3), uint16(r.Status))
	b, err := appendHeaders(b, r.Headers)
	if err != nil {
		return nil, err
	}
	if len(b) > maxFramePayload {
		return nil, ErrTooLarge
	}
	return b, nil
}

func DecodeResponse(payload []byte) (Response, error) {
	c := &cursor{b: payload}

	status, err := c.u16()
	if err != nil {
		return Response{}, err
	}
	if !validStatus(int(status)) {
		return Response{}, malformed("status %d is outside 100..599", status)
	}
	headers, err := readHeaders(c)
	if err != nil {
		return Response{}, err
	}
	if err := c.finish(); err != nil {
		return Response{}, err
	}
	return Response{Status: int(status), Headers: headers}, nil
}

type ErrorCode uint16

const (
	CodeFrameTooLarge   ErrorCode = 1
	CodeInvalidStreamID ErrorCode = 2
	CodeProtocolError   ErrorCode = 3
)

// ConnError is the payload of an ERROR frame. The message is for people
// reading logs; nothing may branch on it.
type ConnError struct {
	Code    ErrorCode
	Message string
}

func (e ConnError) Encode() ([]byte, error) {
	if 4+len(e.Message) > maxFramePayload {
		return nil, ErrTooLarge
	}
	b := appendU16(make([]byte, 0, 4+len(e.Message)), uint16(e.Code))
	b = appendU16(b, uint16(len(e.Message)))
	return append(b, e.Message...), nil
}

// DecodeConnError is forgiving about the code (unknown values are kept) because
// the receiver closes the connection either way.
func DecodeConnError(payload []byte) (ConnError, error) {
	c := &cursor{b: payload}
	code, err := c.u16()
	if err != nil {
		return ConnError{}, err
	}
	msgLen, err := c.u16()
	if err != nil {
		return ConnError{}, err
	}
	msg, err := c.take(int(msgLen))
	if err != nil {
		return ConnError{}, err
	}
	if err := c.finish(); err != nil {
		return ConnError{}, err
	}
	return ConnError{Code: ErrorCode(code), Message: string(msg)}, nil
}
