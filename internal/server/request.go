package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
)

const serverName = "bserve/1"

// bodyError marks a failure after the response headers went out. Unlike a
// plain write error the connection is still healthy enough to say so.
type bodyError struct{ err error }

func (e *bodyError) Error() string { return "sending body: " + e.err.Error() }
func (e *bodyError) Unwrap() error { return e.err }

// serveRequest answers one REQUEST frame. A returned error means the
// connection can no longer be trusted; anything a client did wrong is
// answered with a status instead.
func (s *Server) serveRequest(conn net.Conn, f frame.Frame) error {
	// Flags on a request are checked strictly so a broken client is caught
	// straight away rather than working by accident.
	if f.Flags&^frame.FlagEndStream != 0 || !f.Flags.Has(frame.FlagEndStream) {
		return s.reject(conn, f.StreamID, "request flags are wrong")
	}

	req, err := protocol.DecodeRequest(f.Payload)
	if err != nil {
		return s.reject(conn, f.StreamID, err.Error())
	}

	file, info, err := s.res.open(req.Path)
	switch {
	case errors.Is(err, errBadPath):
		return s.reject(conn, f.StreamID, "path not allowed")
	case errors.Is(err, errNotFound):
		s.logf("GET %s -> 404", printable(req.Path))
		return s.sendStatus(conn, f.StreamID, 404)
	case err != nil:
		s.logf("GET %s -> 500: %v", printable(req.Path), err)
		return s.sendStatus(conn, f.StreamID, 500)
	}
	defer file.Close()

	n, err := s.sendFile(conn, f.StreamID, file, info.Size(), contentType(info.Name()))
	if err != nil {
		return err
	}
	s.logf("GET %s -> 200 (%d bytes)", printable(req.Path), n)
	return nil
}

func (s *Server) reject(conn net.Conn, stream uint32, why string) error {
	s.logf("%v: bad request: %s", conn.RemoteAddr(), why)
	return s.sendStatus(conn, stream, 400)
}

// sendStatus sends a response with no body.
func (s *Server) sendStatus(conn net.Conn, stream uint32, status int) error {
	return s.sendResponse(conn, stream, status, []protocol.Header{
		protocol.NewHeader(protocol.HeaderContentLength, "0"),
		protocol.NewHeader(protocol.HeaderServer, serverName),
	}, true)
}

func (s *Server) sendResponse(conn net.Conn, stream uint32, status int, headers []protocol.Header, endStream bool) error {
	payload, err := protocol.Response{Status: status, Headers: headers}.Encode()
	if err != nil {
		return err
	}
	f := frame.Frame{Type: frame.TypeResponse, StreamID: stream, Payload: payload}
	if endStream {
		f.Flags = frame.FlagEndStream
	}
	return s.write(conn, f)
}

// sendFile streams size bytes from file. The size comes from the open handle,
// so the length we announce is the length we promise to deliver even if the
// file changes on disk afterwards.
func (s *Server) sendFile(conn net.Conn, stream uint32, file *os.File, size int64, ctype string) (int64, error) {
	headers := []protocol.Header{
		protocol.NewHeader(protocol.HeaderContentType, ctype),
		protocol.NewHeader(protocol.HeaderContentLength, strconv.FormatInt(size, 10)),
		protocol.NewHeader(protocol.HeaderServer, serverName),
	}
	if err := s.sendResponse(conn, stream, 200, headers, size == 0); err != nil {
		return 0, err
	}

	buf := make([]byte, frame.MaxPayload)
	remaining := size
	for remaining > 0 {
		chunk := buf[:min(int64(len(buf)), remaining)]
		if _, err := io.ReadFull(file, chunk); err != nil {
			return size - remaining, &bodyError{fmt.Errorf("file ended early: %w", err)}
		}
		remaining -= int64(len(chunk))

		f := frame.Frame{Type: frame.TypeData, StreamID: stream, Payload: chunk}
		if remaining == 0 {
			f.Flags = frame.FlagEndStream
		}
		if err := s.write(conn, f); err != nil {
			return size - remaining, err
		}
	}
	return size, nil
}

// printable keeps hostile paths from smuggling control characters or
// enormous strings into the log.
func printable(p string) string {
	if len(p) > 120 {
		p = p[:120] + "..."
	}
	return strconv.Quote(p)
}
