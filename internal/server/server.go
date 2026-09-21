// Package server answers file requests arriving as frames on persistent TCP
// connections.
package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
)

const (
	defaultIdleTimeout  = 30 * time.Second
	defaultWriteTimeout = 30 * time.Second

	// After sending a connection-level error we keep reading briefly. If we
	// closed with unread data pending, the OS would reset the connection and
	// the peer might never see the error. Both bounds stop a hostile peer from
	// keeping us here.
	drainTime  = time.Second
	drainBytes = 64 << 10
)

type Config struct {
	Root         string
	IdleTimeout  time.Duration // how long a connection may sit silent between frames
	WriteTimeout time.Duration // how long a single frame may take to send
	Logger       *log.Logger   // nil silences logging
}

type Server struct {
	res          *resolver
	idleTimeout  time.Duration
	writeTimeout time.Duration
	logger       *log.Logger

	mu      sync.Mutex
	closed  bool
	lis     net.Listener
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
	current atomic.Int64
}

func New(cfg Config) (*Server, error) {
	res, err := newResolver(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("document root: %w", err)
	}
	s := &Server{
		res:          res,
		idleTimeout:  cfg.IdleTimeout,
		writeTimeout: cfg.WriteTimeout,
		logger:       cfg.Logger,
		conns:        make(map[net.Conn]struct{}),
	}
	if s.idleTimeout <= 0 {
		s.idleTimeout = defaultIdleTimeout
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = defaultWriteTimeout
	}
	return s, nil
}

// Serve accepts connections until the listener fails or Close is called, in
// which case it returns nil.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		l.Close()
		return nil
	}
	s.lis = l
	s.mu.Unlock()

	for {
		conn, err := l.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Running out of file handles and similar hiccups pass; spinning
			// on them would only burn CPU.
			s.logf("accept failed: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !s.track(conn) {
			conn.Close()
			return nil
		}
		go s.handle(conn)
	}
}

// Close stops accepting, drops open connections and waits for their handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	lis := s.lis
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()

	if lis != nil {
		lis.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	s.current.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.current.Add(-1)
	s.wg.Done()
}

func (s *Server) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

// handle owns one connection for its whole life. It only moves frames around;
// what requests mean lives in request.go.
func (s *Server) handle(conn net.Conn) {
	defer s.untrack(conn)
	defer conn.Close()

	peer := conn.RemoteAddr()
	s.logf("%v connected", peer)
	defer s.logf("%v disconnected", peer)

	br := bufio.NewReader(conn)
	for {
		conn.SetReadDeadline(time.Now().Add(s.idleTimeout))
		f, err := frame.Read(br)
		if err != nil {
			s.onReadError(conn, err)
			return
		}

		switch f.Type {
		case frame.TypeRequest:
			if err := s.serveRequest(conn, f); err != nil {
				s.onServeError(conn, err)
				return
			}
		case frame.TypeResponse, frame.TypeData:
			// Clients have no business sending these, but the frame itself
			// was well formed, so we answer that stream and carry on.
			if err := s.sendStatus(conn, f.StreamID, 400); err != nil {
				s.logf("%v: %v", conn.RemoteAddr(), err)
				return
			}
		case frame.TypeError:
			s.logf("%v sent an error frame, closing", conn.RemoteAddr())
			return
		}
	}
}

func (s *Server) onReadError(conn net.Conn, err error) {
	var oversize *frame.OversizeError
	switch {
	case errors.Is(err, io.EOF):
		// The client hung up between requests, which is how it says goodbye.
	case errors.Is(err, frame.ErrTruncated):
		s.logf("%v: connection ended mid-frame", conn.RemoteAddr())
	case errors.Is(err, frame.ErrStreamZero):
		s.fail(conn, protocol.CodeInvalidStreamID, "stream id 0 is not valid here")
	case errors.As(err, &oversize):
		// Nobody answers an error frame, so an oversized one just ends things.
		if oversize.Type == frame.TypeError {
			s.logf("%v: oversized error frame, closing", conn.RemoteAddr())
			return
		}
		s.fail(conn, protocol.CodeFrameTooLarge, "frame payload is too large")
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			s.logf("%v: idle for too long, closing", conn.RemoteAddr())
		} else {
			s.logf("%v: read failed: %v", conn.RemoteAddr(), err)
		}
	}
}

func (s *Server) onServeError(conn net.Conn, err error) {
	var broken *bodyError
	if errors.As(err, &broken) {
		// The response headers are already out, so the only honest signal
		// left is an error frame followed by closing.
		s.logf("%v: %v", conn.RemoteAddr(), err)
		s.fail(conn, protocol.CodeProtocolError, "could not finish sending the response")
		return
	}
	s.logf("%v: write failed: %v", conn.RemoteAddr(), err)
}

// fail reports a connection-level problem, then waits a little for the peer to
// finish talking before closing so the report survives.
func (s *Server) fail(conn net.Conn, code protocol.ErrorCode, msg string) {
	s.logf("%v: %s", conn.RemoteAddr(), msg)

	payload, err := protocol.ConnError{Code: code, Message: msg}.Encode()
	if err != nil {
		return
	}
	if err := s.write(conn, frame.Frame{Type: frame.TypeError, Payload: payload}); err != nil {
		return
	}
	drain(conn)
}

// drain half-closes our side, then discards whatever the peer still sends, up
// to a fixed time and size.
func drain(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	conn.SetReadDeadline(time.Now().Add(drainTime))
	io.Copy(io.Discard, io.LimitReader(conn, drainBytes))
}

func (s *Server) write(conn net.Conn, f frame.Frame) error {
	conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	return frame.Write(conn, f)
}
