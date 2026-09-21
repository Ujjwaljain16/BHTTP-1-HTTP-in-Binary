// Package client sends requests over one persistent connection and checks the
// answers the way any well-behaved peer must.
package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
)

const (
	defaultTimeout = 30 * time.Second
	drainTime      = time.Second
	drainBytes     = 64 << 10
)

// ErrBroken is returned once a failure has ended the connection. The client
// never reconnects on its own, because silently starting over would hide
// exactly the problems this protocol exists to expose.
var ErrBroken = errors.New("client: the connection has already failed")

// RemoteError is a connection-level error the server reported.
type RemoteError struct {
	Code    protocol.ErrorCode
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("server reported error %d: %s", e.Code, e.Message)
}

type Result struct {
	Status    int
	Headers   []protocol.Header
	BodyBytes int64
}

type Client struct {
	// Timeout bounds how long any single read or write may take.
	Timeout time.Duration

	conn      net.Conn
	br        *bufio.Reader
	next      uint32
	exhausted bool
	broken    bool
}

// New takes ownership of conn.
func New(conn net.Conn) *Client {
	return &Client{Timeout: defaultTimeout, conn: conn, br: bufio.NewReader(conn), next: 1}
}

func (c *Client) Close() error { return c.conn.Close() }

// Get requests path, sending any extra request headers, and streams the body
// into body as it arrives. Any HTTP-style
// status, including 404, is a normal result; errors mean the exchange itself
// went wrong and the connection is no longer usable.
func (c *Client) Get(path string, body io.Writer, headers ...protocol.Header) (Result, error) {
	if c.broken {
		return Result{}, ErrBroken
	}
	if c.exhausted {
		return Result{}, c.fail(errors.New("all stream ids on this connection are used up"))
	}

	id := c.next
	if id == 0xFFFFFFFF {
		c.exhausted = true
	} else {
		c.next++
	}

	payload, err := protocol.Request{Method: protocol.MethodGet, Path: path, Headers: headers}.Encode()
	if err != nil {
		// Nothing was sent, so the connection is still fine.
		return Result{}, fmt.Errorf("cannot build request: %w", err)
	}
	req := frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: id, Payload: payload}
	c.conn.SetWriteDeadline(time.Now().Add(c.Timeout))
	if err := frame.Write(c.conn, req); err != nil {
		return Result{}, c.fail(fmt.Errorf("sending request: %w", err))
	}

	res, err := c.readResponse(id, body)
	if err != nil {
		return Result{}, c.fail(err)
	}
	return res, nil
}

func (c *Client) readResponse(id uint32, body io.Writer) (Result, error) {
	f, err := c.readFrame()
	if err != nil {
		return Result{}, err
	}
	if err := c.expect(f, frame.TypeResponse, id); err != nil {
		return Result{}, err
	}
	resp, err := protocol.DecodeResponse(f.Payload)
	if err != nil {
		return Result{}, fmt.Errorf("unreadable response: %w", err)
	}

	res := Result{Status: resp.Status, Headers: resp.Headers}
	done := f.Flags.Has(frame.FlagEndStream)
	for !done {
		d, err := c.readFrame()
		if err != nil {
			return Result{}, err
		}
		if err := c.expect(d, frame.TypeData, id); err != nil {
			return Result{}, err
		}
		if _, err := body.Write(d.Payload); err != nil {
			return Result{}, fmt.Errorf("writing body: %w", err)
		}
		res.BodyBytes += int64(len(d.Payload))
		done = d.Flags.Has(frame.FlagEndStream)
	}

	if err := checkLength(resp.Headers, res.BodyBytes); err != nil {
		return Result{}, err
	}
	return res, nil
}

// expect confirms a frame is the kind we are waiting for, on the right stream.
func (c *Client) expect(f frame.Frame, want frame.Type, id uint32) error {
	switch f.Type {
	case frame.TypeError:
		return remoteError(f)
	case frame.TypeRequest:
		// Servers do not send requests. There is no stream to blame, so the
		// whole connection gets the error.
		c.sendError(protocol.CodeProtocolError, "unexpected request from server")
		return errors.New("server sent a request frame")
	}
	if f.Type != want || f.StreamID != id {
		return fmt.Errorf("expected %s on stream %d but got type %d on stream %d", typeLabel(want), id, f.Type, f.StreamID)
	}
	return nil
}

func typeLabel(t frame.Type) string {
	if t == frame.TypeResponse {
		return "a response"
	}
	return "data"
}

func remoteError(f frame.Frame) error {
	ce, err := protocol.DecodeConnError(f.Payload)
	if err != nil {
		// A garbled error frame is still an error frame.
		return &RemoteError{Message: "unreadable error message"}
	}
	return &RemoteError{Code: ce.Code, Message: ce.Message}
}

// readFrame reads one frame and turns framing faults into the right error
// frame back to the server.
func (c *Client) readFrame() (frame.Frame, error) {
	c.conn.SetReadDeadline(time.Now().Add(c.Timeout))
	f, err := frame.Read(c.br)
	if err == nil {
		return f, nil
	}

	var oversize *frame.OversizeError
	switch {
	case errors.Is(err, frame.ErrStreamZero):
		c.sendError(protocol.CodeInvalidStreamID, "stream id 0 is not valid here")
		return frame.Frame{}, errors.New("server used stream id 0")
	case errors.As(err, &oversize):
		if oversize.Type != frame.TypeError {
			c.sendError(protocol.CodeFrameTooLarge, "frame payload is too large")
		}
		return frame.Frame{}, fmt.Errorf("server sent a frame declaring %d payload bytes", oversize.Length)
	case errors.Is(err, io.EOF), errors.Is(err, frame.ErrTruncated):
		return frame.Frame{}, errors.New("connection closed before the response was complete")
	}
	return frame.Frame{}, fmt.Errorf("reading response: %w", err)
}

// checkLength enforces the length the server promised. Repeats are allowed on
// the wire, so every one of them has to agree with what arrived.
func checkLength(headers []protocol.Header, got int64) error {
	for _, h := range headers {
		if h.Name != protocol.HeaderContentLength {
			continue
		}
		v := string(h.Value)
		if len(v) == 0 || len(v) > 19 {
			return fmt.Errorf("content-length %q is not a valid number", v)
		}
		for i := 0; i < len(v); i++ {
			if v[i] < '0' || v[i] > '9' {
				return fmt.Errorf("content-length %q is not a valid number", v)
			}
		}
		want, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("content-length %q is not a valid number", v)
		}
		if want != got {
			return fmt.Errorf("server promised %d body bytes but sent %d", want, got)
		}
	}
	return nil
}

// fail ends the connection for good.
func (c *Client) fail(err error) error {
	c.broken = true
	c.conn.Close()
	return err
}

// sendError is best effort: the connection is about to be dropped anyway.
func (c *Client) sendError(code protocol.ErrorCode, msg string) {
	payload, err := protocol.ConnError{Code: code, Message: msg}.Encode()
	if err != nil {
		return
	}
	c.conn.SetWriteDeadline(time.Now().Add(c.Timeout))
	if frame.Write(c.conn, frame.Frame{Type: frame.TypeError, Payload: payload}) != nil {
		return
	}
	// Same courtesy the server extends: finish sending, then let the peer
	// finish talking, so a reset does not swallow the error we just sent.
	if cw, ok := c.conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	c.conn.SetReadDeadline(time.Now().Add(drainTime))
	io.Copy(io.Discard, io.LimitReader(c.br, drainBytes))
}
