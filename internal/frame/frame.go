// Package frame reads and writes the length-prefixed frames that carry every
// message on a connection. It knows nothing about what the payloads mean.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	HeaderSize = 12
	MaxPayload = 16384
)

type Type uint8

const (
	TypeRequest  Type = 0x01
	TypeResponse Type = 0x02
	TypeData     Type = 0x03
	TypeError    Type = 0x04
)

func (t Type) known() bool { return t >= TypeRequest && t <= TypeError }

// Only ERROR frames may use stream zero, because they describe the whole
// connection rather than one exchange.
func (t Type) needsStream() bool { return t >= TypeRequest && t <= TypeData }

type Flags uint8

const FlagEndStream Flags = 0x01

func (f Flags) Has(mask Flags) bool { return f&mask == mask }

// Frame deliberately has no field for the reserved header bytes: senders
// always write zero and receivers must never act on them, so exposing them
// would only invite misuse.
type Frame struct {
	Type     Type
	Flags    Flags
	StreamID uint32
	Payload  []byte
}

var (
	ErrTruncated       = errors.New("frame: connection ended in the middle of a frame")
	ErrPayloadTooLarge = errors.New("frame: payload is larger than the maximum frame size")
	ErrStreamZero      = errors.New("frame: stream id 0 is not valid for this frame type")
)

// OversizeError carries the frame type because the right reaction differs:
// an oversized ERROR frame is simply dropped along with the connection, while
// any other type deserves an ERROR frame in reply first.
type OversizeError struct {
	Type   Type
	Length uint32
}

func (e *OversizeError) Error() string {
	return fmt.Sprintf("frame: declared payload of %d bytes exceeds the %d byte limit", e.Length, MaxPayload)
}

// Encode returns the frame exactly as it goes on the wire. It refuses to
// build frames a conforming peer would have to reject.
func Encode(f Frame) ([]byte, error) {
	if len(f.Payload) > MaxPayload {
		return nil, ErrPayloadTooLarge
	}
	if f.Type.needsStream() && f.StreamID == 0 {
		return nil, ErrStreamZero
	}
	b := make([]byte, HeaderSize+len(f.Payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(f.Payload)))
	b[4] = byte(f.Type)
	b[5] = byte(f.Flags)
	// b[6:8] is the reserved field and stays zero.
	binary.BigEndian.PutUint32(b[8:12], f.StreamID)
	copy(b[HeaderSize:], f.Payload)
	return b, nil
}
