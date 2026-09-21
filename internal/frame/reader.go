package frame

import (
	"encoding/binary"
	"errors"
	"io"
)

// Read returns the next frame of a type this package understands, silently
// stepping over frames of any other type so newer peers can add frame types
// without breaking us.
//
// A clean end of stream between frames comes back as io.EOF. Running out of
// bytes anywhere inside a frame is ErrTruncated. Other errors come from the
// underlying reader untouched.
func Read(r io.Reader) (Frame, error) {
	var hdr [HeaderSize]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return Frame{}, ErrTruncated
			}
			return Frame{}, err
		}

		length := binary.BigEndian.Uint32(hdr[0:4])
		typ := Type(hdr[4])

		// Checked before touching the payload so a hostile length can never
		// make us allocate or wait for gigabytes. Unknown types are held to
		// the same limit because we cannot tell what they would do to memory.
		if length > MaxPayload {
			return Frame{}, &OversizeError{Type: typ, Length: length}
		}

		if !typ.known() {
			if _, err := io.CopyN(io.Discard, r, int64(length)); err != nil {
				return Frame{}, truncatedOr(err)
			}
			continue
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, truncatedOr(err)
		}

		f := Frame{
			Type:     typ,
			Flags:    Flags(hdr[5]),
			StreamID: binary.BigEndian.Uint32(hdr[8:12]),
			Payload:  payload,
		}
		if typ.needsStream() && f.StreamID == 0 {
			return Frame{}, ErrStreamZero
		}
		return f, nil
	}
}

// Any EOF after a header has been read means the peer cut the frame short,
// even when it arrives as a plain io.EOF because zero payload bytes were read.
func truncatedOr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return err
}
