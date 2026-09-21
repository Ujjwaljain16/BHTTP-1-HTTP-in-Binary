package frame

import "io"

// Write sends one complete frame. The header and payload go out in a single
// buffer so small frames do not end up as two tiny TCP segments, and a short
// write is retried until everything is out or the writer fails.
func Write(w io.Writer, f Frame) error {
	b, err := Encode(f)
	if err != nil {
		return err
	}
	return writeAll(w, b)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		// A writer that reports success but makes no progress would spin
		// forever here.
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
