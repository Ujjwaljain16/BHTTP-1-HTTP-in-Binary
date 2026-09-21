// Package protocol encodes and decodes the payloads that sit inside frames:
// requests, responses, headers and connection-level errors.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	MaxPathLen        = 4096
	MaxHeaders        = 255
	MaxCustomNameLen  = 255
	maxHeaderValueLen = 0xFFFF
	maxFramePayload   = 16384
)

type Method uint8

const MethodGet Method = 0x01

// ErrMalformed marks a payload that arrived intact inside a valid frame but
// does not follow the message layout. The stream is still in sync, so callers
// can answer it and carry on.
var ErrMalformed = errors.New("malformed message")

// ErrTooLarge means a message we were asked to build cannot fit in one frame.
var ErrTooLarge = errors.New("message does not fit in one frame")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// cursor walks a payload and refuses to read past its end, which is what
// keeps every untrusted length field harmless.
type cursor struct {
	b []byte
}

func (c *cursor) remaining() int { return len(c.b) }

func (c *cursor) take(n int) ([]byte, error) {
	if n < 0 || n > len(c.b) {
		return nil, malformed("need %d more bytes but only %d remain", n, len(c.b))
	}
	out := c.b[:n:n]
	c.b = c.b[n:]
	return out, nil
}

func (c *cursor) u8() (uint8, error) {
	b, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) u16() (uint16, error) {
	b, err := c.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

// finish insists the whole payload was used. Trailing bytes usually mean the
// sender and receiver disagree about the layout.
func (c *cursor) finish() error {
	if len(c.b) != 0 {
		return malformed("%d unexpected bytes after the last field", len(c.b))
	}
	return nil
}

func appendU16(dst []byte, v uint16) []byte {
	return binary.BigEndian.AppendUint16(dst, v)
}

func validUTF8(b []byte) bool { return utf8.Valid(b) }
